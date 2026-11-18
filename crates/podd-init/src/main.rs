//! `podd-init` — CubePod PID 1 In-Guest Persistent RootFS Mounter.
//!
//! Why Rust for `podd-init`?
//! Linux `pivot_root(2)` and `chroot(2)` require a single-threaded process (or careful control
//! over OS threads and file descriptors) before spawning the container runtime. Go's runtime
//! spawns multiple background OS threads before `main()` even starts, which can cause subtle
//! `EINVAL` or leaked root references during `pivot_root`.
//!
//! `podd-init` runs as a tiny, zero-GC, single-threaded Rust binary at PID 1
//! (injected at runtime into `/opt/cubepod/bin/podd-init` via an `initContainer` shared `emptyDir`
//! volume, never baked into user sandbox images):
//! 1. Verifies/formats (`mkfs.ext4`) or auto-heals (`e2fsck -p`) `/dev/cubepod-rootfs` and `/dev/cubepod-workspace`.
//! 2. Mounts the persistent RootFS Upper block device and constructs a Guest Kernel `OverlayFS`
//!    merging the read-only Base Image (`/`) with the persistent `upperdir` and `workdir`.
//! 3. Mounts the persistent Workspace block device at `<merged>/workspace` and sets ownership (`1000:1000`).
//! 4. Bind-mounts `/proc`, `/sys`, `/dev`, `/run`, `/opt/cubepod` (the injected runtime binary volume),
//!    and K8s `/etc/resolv.conf`, `/etc/hosts`, `/etc/hostname`.
//! 5. Calls `pivot_root(2)`, detaches `/.old_root`, and `execvp(3)`s into `/opt/cubepod/bin/podd`.

use std::collections::HashSet;
use std::env;
use std::ffi::CString;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode};
use std::time::Instant;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InitConfig {
    pub rootfs_device: PathBuf,
    pub workspace_device: PathBuf,
    pub upper_mount_path: PathBuf,
    pub merged_root_path: PathBuf,
    pub workspace_dir: PathBuf,
    pub default_uid: u32,
    pub default_gid: u32,
    pub init_state_path: PathBuf,
    pub skip_pivot: bool,
    pub next_argv: Vec<String>,
}

impl InitConfig {
    pub fn from_env_and_args(args: Vec<String>) -> Self {
        let next_argv = if args.is_empty() {
            vec!["/opt/cubepod/bin/podd".to_string()]
        } else {
            args
        };

        let default_uid = env::var("CUBEPOD_UID")
            .ok()
            .and_then(|v| v.parse::<u32>().ok())
            .unwrap_or(1000);

        let default_gid = env::var("CUBEPOD_GID")
            .ok()
            .and_then(|v| v.parse::<u32>().ok())
            .unwrap_or(1000);

        let init_state_path = PathBuf::from(
            env::var("CUBEPOD_INIT_STATE_PATH")
                .unwrap_or_else(|_| "/run/cubepod/init-state.json".into()),
        );

        Self {
            rootfs_device: PathBuf::from(
                env::var("CUBEPOD_ROOTFS_DEV").unwrap_or_else(|_| "/dev/cubepod-rootfs".into()),
            ),
            workspace_device: PathBuf::from(
                env::var("CUBEPOD_WORKSPACE_DEV")
                    .unwrap_or_else(|_| "/dev/cubepod-workspace".into()),
            ),
            upper_mount_path: PathBuf::from(
                env::var("CUBEPOD_UPPER_MOUNT").unwrap_or_else(|_| "/mnt/cubepod-upper".into()),
            ),
            merged_root_path: PathBuf::from(
                env::var("CUBEPOD_MERGED_ROOT").unwrap_or_else(|_| "/mnt/cubepod-merged".into()),
            ),
            workspace_dir: PathBuf::from("/workspace"),
            default_uid,
            default_gid,
            init_state_path,
            skip_pivot: env::var("CUBEPOD_SKIP_PIVOT").as_deref() == Ok("1"),
            next_argv,
        }
    }
}

/// Action performed on a raw block volume during initial verification.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum VolumeAction {
    Formatted,
    Healed,
    Mounted,
    Skipped,
}

impl VolumeAction {
    pub fn as_str(&self) -> &'static str {
        match self {
            VolumeAction::Formatted => "formatted",
            VolumeAction::Healed => "healed",
            VolumeAction::Mounted => "mounted",
            VolumeAction::Skipped => "skipped",
        }
    }
}

impl std::fmt::Display for VolumeAction {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{}", self.as_str())
    }
}

/// In-guest initialization marker written to `/run/cubepod/init-state.json`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InitState {
    pub pivoted: bool,
    pub rootfs_device: String,
    pub rootfs_action: VolumeAction,
    pub workspace_device: String,
    pub workspace_action: VolumeAction,
    pub duration_ms: u64,
    pub e2b_bind_duration_ms: u64,
}

impl InitState {
    pub fn to_json(&self) -> String {
        format!(
            "{{\n  \"pivoted\": {},\n  \"rootfs_device\": \"{}\",\n  \"rootfs_action\": \"{}\",\n  \"workspace_device\": \"{}\",\n  \"workspace_action\": \"{}\",\n  \"duration_ms\": {},\n  \"e2b_bind_duration_ms\": {}\n}}\n",
            self.pivoted,
            self.rootfs_device,
            self.rootfs_action.as_str(),
            self.workspace_device,
            self.workspace_action.as_str(),
            self.duration_ms,
            self.e2b_bind_duration_ms
        )
    }

    pub fn write_to_file(&self, path: &Path) -> Result<(), String> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|e| format!("failed to create dir {}: {e}", parent.display()))?;
        }
        fs::write(path, self.to_json())
            .map_err(|e| format!("failed to write init state to {}: {e}", path.display()))?;
        Ok(())
    }
}

/// Ensures `device` has a valid `ext4` filesystem; formats with `mkfs.ext4` if blank,
/// or runs `e2fsck -p` to auto-heal after unclean VM shutdowns.
pub fn ensure_ext4(device: &Path, label: &str) -> Result<VolumeAction, String> {
    if !device.exists() {
        return Err(format!("block device {} does not exist", device.display()));
    }

    let blkid = Command::new("blkid")
        .args(["-o", "value", "-s", "TYPE"])
        .arg(device)
        .output()
        .map_err(|e| format!("failed to run blkid on {}: {e}", device.display()))?;

    let fstype = String::from_utf8_lossy(&blkid.stdout).trim().to_string();
    if !blkid.status.success() || fstype.is_empty() {
        eprintln!(
            "[podd-init] formatting fresh raw block volume {} as ext4 (label={})",
            device.display(),
            label
        );
        let status = Command::new("mkfs.ext4")
            .args([
                "-F",
                "-L",
                label,
                "-m",
                "0",
                "-E",
                "lazy_itable_init=0,lazy_journal_init=0",
            ])
            .arg(device)
            .status()
            .map_err(|e| format!("failed to spawn mkfs.ext4: {e}"))?;

        if !status.success() {
            return Err(format!(
                "mkfs.ext4 exited with {} on {}",
                status,
                device.display()
            ));
        }
        return Ok(VolumeAction::Formatted);
    }

    eprintln!(
        "[podd-init] verifying existing ext4 volume {} with e2fsck -p",
        device.display()
    );
    let mut last_code = None;
    for attempt in 0..10 {
        if attempt > 0 {
            std::thread::sleep(std::time::Duration::from_millis(500));
        }
        let status = Command::new("e2fsck")
            .arg("-p")
            .arg(device)
            .status()
            .map_err(|e| format!("failed to spawn e2fsck: {e}"))?;

        // e2fsck exit code 0 = no errors, 1 = filesystem errors corrected, 2 = corrected (reboot suggested)
        match status.code() {
            Some(0) => return Ok(VolumeAction::Mounted),
            Some(1..=2) => return Ok(VolumeAction::Healed),
            Some(8) => {
                // Operational error, often EBUSY / device in use while udev or previous container finishes releasing device
                eprintln!(
                    "[podd-init] e2fsck reported device in use on {} (attempt {}), retrying...",
                    device.display(),
                    attempt + 1
                );
                last_code = Some(8);
                continue;
            }
            other => {
                return Err(format!(
                    "e2fsck -p failed on {} with exit code {:?}",
                    device.display(),
                    other
                ));
            }
        }
    }

    Err(format!(
        "e2fsck -p failed on {} after retries with exit code {:?}",
        device.display(),
        last_code
    ))
}

pub fn setup_and_pivot(cfg: &InitConfig) -> Result<(), String> {
    let start = Instant::now();
    let ws_str = cfg.workspace_dir.to_string_lossy();
    let e2b_mounts = load_e2b_mounts(&ws_str)?;

    if cfg.skip_pivot || !cfg.rootfs_device.exists() {
        if cfg.skip_pivot {
            eprintln!("[podd-init] CUBEPOD_SKIP_PIVOT=1 set, skipping block mount and pivot_root");
        } else {
            eprintln!(
                "[podd-init] no raw block device at {}, continuing on standard rootfs",
                cfg.rootfs_device.display()
            );
        }
        let _ = fs::create_dir_all(&cfg.workspace_dir);
        let _ = chown_path(&cfg.workspace_dir, cfg.default_uid, cfg.default_gid);
        let _ = fs::set_permissions(&cfg.workspace_dir, fs::Permissions::from_mode(0o777));
        let mut e2b_bind_duration_ms = 0u64;
        if !e2b_mounts.is_empty() {
            let bind_start = Instant::now();
            mount_e2b_volumes(
                &e2b_mounts,
                Path::new("/"),
                cfg.default_uid,
                cfg.default_gid,
                true,
            )?;
            e2b_bind_duration_ms = bind_start.elapsed().as_millis() as u64;
        }
        let state = InitState {
            pivoted: false,
            rootfs_device: cfg.rootfs_device.to_string_lossy().to_string(),
            rootfs_action: VolumeAction::Skipped,
            workspace_device: cfg.workspace_device.to_string_lossy().to_string(),
            workspace_action: VolumeAction::Skipped,
            duration_ms: start.elapsed().as_millis() as u64,
            e2b_bind_duration_ms,
        };
        if let Err(e) = state.write_to_file(&cfg.init_state_path) {
            eprintln!(
                "[podd-init] warning: failed to write init state to {}: {e}",
                cfg.init_state_path.display()
            );
        }
        return Ok(());
    }

    // Set root mount propagation to MS_PRIVATE | MS_REC before assembling mounts to allow pivot_root.
    make_mount_private(Path::new("/"))?;

    // 1. Heal or format RootFS Upper block device.
    let rootfs_action = ensure_ext4(&cfg.rootfs_device, "cubepod-rootfs")?;

    // 2. Mount RootFS Upper block device and create upper/ and work/ directories.
    fs::create_dir_all(&cfg.upper_mount_path)
        .map_err(|e| format!("mkdir {}: {e}", cfg.upper_mount_path.display()))?;
    mount_ext4(&cfg.rootfs_device, &cfg.upper_mount_path)?;

    let upper_dir = cfg.upper_mount_path.join("upper");
    let work_dir = cfg.upper_mount_path.join("work");
    for d in [&upper_dir, &work_dir, &cfg.merged_root_path] {
        fs::create_dir_all(d).map_err(|e| format!("mkdir {}: {e}", d.display()))?;
    }

    // 3. Assemble OverlayFS combining Base Image (/) as lowerdir and block PVC as upperdir.
    let overlay_opts = format!(
        "lowerdir=/,upperdir={},workdir={}",
        upper_dir.display(),
        work_dir.display()
    );
    eprintln!(
        "[podd-init] mounting persistent OverlayFS at {} ({})",
        cfg.merged_root_path.display(),
        overlay_opts
    );
    let status = Command::new("mount")
        .args(["-t", "overlay", "overlay", "-o", &overlay_opts])
        .arg(&cfg.merged_root_path)
        .status()
        .map_err(|e| format!("failed to spawn mount overlay: {e}"))?;
    if !status.success() {
        return Err(format!("overlayfs mount failed with status {status}"));
    }

    // 4. Mount Workspace block device at <merged>/workspace and chown to default_uid:default_gid.
    let rel_ws = cfg
        .workspace_dir
        .strip_prefix("/")
        .unwrap_or(&cfg.workspace_dir);
    let merged_workspace = cfg.merged_root_path.join(rel_ws);
    fs::create_dir_all(&merged_workspace)
        .map_err(|e| format!("mkdir {}: {e}", merged_workspace.display()))?;

    let workspace_action = if cfg.workspace_device.exists() {
        let action = ensure_ext4(&cfg.workspace_device, "cubepod-ws")?;
        mount_ext4(&cfg.workspace_device, &merged_workspace)?;
        action
    } else {
        VolumeAction::Skipped
    };
    chown_path(&merged_workspace, cfg.default_uid, cfg.default_gid)?;

    // 5. Bind-mount virtual filesystems, the injected /opt/cubepod runtime volume,
    // and K8s networking configs into the merged root so podd remains accessible after pivot_root
    // without polluting the user's persistent RootFS upperdir.
    for sys_dir in ["proc", "sys", "dev", "run", "opt/cubepod", "etc/cubepod"] {
        let src = Path::new("/").join(sys_dir);
        if src.exists() {
            let dst = cfg.merged_root_path.join(sys_dir);
            let _ = fs::create_dir_all(&dst);
            let _ = Command::new("mount")
                .arg("--rbind")
                .arg(&src)
                .arg(&dst)
                .status();
        }
    }

    for etc_file in ["etc/resolv.conf", "etc/hosts", "etc/hostname"] {
        let src = Path::new("/").join(etc_file);
        if src.exists() {
            let dst = cfg.merged_root_path.join(etc_file);
            if let Some(parent) = dst.parent() {
                let _ = fs::create_dir_all(parent);
            }
            let _ = fs::OpenOptions::new()
                .create(true)
                .truncate(false)
                .write(true)
                .open(&dst);
            let _ = Command::new("mount")
                .arg("--bind")
                .arg(&src)
                .arg(&dst)
                .status();
        }
    }

    let extra_ca = Path::new("/etc/cubepod/ca/mitm-ca.crt");
    let ca_bundle_dst = cfg
        .merged_root_path
        .join("etc/ssl/certs/ca-certificates.crt");
    let base_bundle = ca_bundle_dst.exists().then_some(ca_bundle_dst.as_path());
    let composed_ca = Path::new("/run/cubepod/ca-certificates.crt");
    match compose_ca_bundle(base_bundle, extra_ca, composed_ca) {
        Ok(true) => {
            if !ca_bundle_dst.exists() {
                if let Some(parent) = ca_bundle_dst.parent() {
                    let _ = fs::create_dir_all(parent);
                }
                let _ = fs::OpenOptions::new()
                    .create(true)
                    .truncate(false)
                    .write(true)
                    .open(&ca_bundle_dst);
            }
            let _ = Command::new("mount")
                .arg("--bind")
                .arg(composed_ca)
                .arg(&ca_bundle_dst)
                .status();
        }
        Ok(false) => {}
        Err(err) => {
            eprintln!("[podd-init] warning: failed to compose CA bundle: {err}");
        }
    }

    let mut e2b_bind_duration_ms = 0u64;
    if !e2b_mounts.is_empty() {
        let bind_start = Instant::now();
        mount_e2b_volumes(
            &e2b_mounts,
            &cfg.merged_root_path,
            cfg.default_uid,
            cfg.default_gid,
            false,
        )?;
        for m in &e2b_mounts {
            unmount_lazy_ignore_missing(
                &cfg.merged_root_path
                    .join("run/cubepod/volumes")
                    .join(&m.volume_id),
            );
        }
        e2b_bind_duration_ms = bind_start.elapsed().as_millis() as u64;
    }

    // 6. Perform pivot_root(2) into the merged root.
    let old_root = cfg.merged_root_path.join(".old_root");
    fs::create_dir_all(&old_root).map_err(|e| format!("mkdir .old_root: {e}"))?;
    let _ = fs::set_permissions(&old_root, fs::Permissions::from_mode(0o700));

    if let Err(err) = pivot_root(&cfg.merged_root_path, &old_root) {
        eprintln!("[podd-init] pivot_root returned ({err}), falling back to chroot");
        chroot(&cfg.merged_root_path)?;
    } else {
        env::set_current_dir("/").map_err(|e| format!("chdir /: {e}"))?;
        unmount_lazy(Path::new("/.old_root"))?;
        let _ = fs::remove_dir("/.old_root");
    }

    if env::set_current_dir(&cfg.workspace_dir).is_err() {
        let _ = env::set_current_dir("/");
    }

    // 7. Write init state marker
    let state = InitState {
        pivoted: true,
        rootfs_device: cfg.rootfs_device.to_string_lossy().to_string(),
        rootfs_action,
        workspace_device: cfg.workspace_device.to_string_lossy().to_string(),
        workspace_action,
        duration_ms: start.elapsed().as_millis() as u64,
        e2b_bind_duration_ms,
    };
    if let Err(e) = state.write_to_file(&cfg.init_state_path) {
        eprintln!(
            "[podd-init] warning: failed to write init state to {}: {e}",
            cfg.init_state_path.display()
        );
    }

    eprintln!(
        "[podd-init] persistent OverlayFS root active (workdir={})",
        cfg.workspace_dir.display()
    );
    Ok(())
}

fn make_mount_private(target: &Path) -> Result<(), String> {
    let c_target = CString::new(target.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid target path: {e}"))?;
    let rc = unsafe {
        libc::mount(
            std::ptr::null(),
            c_target.as_ptr(),
            std::ptr::null(),
            (libc::MS_PRIVATE | libc::MS_REC) as libc::c_ulong,
            std::ptr::null(),
        )
    };
    if rc != 0 {
        return Err(format!(
            "mount private on {} failed: {}",
            target.display(),
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn mount_ext4(device: &Path, target: &Path) -> Result<(), String> {
    let status = Command::new("mount")
        .args(["-t", "ext4", "-o", "noatime"])
        .arg(device)
        .arg(target)
        .status()
        .map_err(|e| format!("failed to spawn mount ext4: {e}"))?;
    if !status.success() {
        return Err(format!(
            "mount ext4 {} -> {} failed with {status}",
            device.display(),
            target.display()
        ));
    }
    Ok(())
}

fn chown_path(path: &Path, uid: u32, gid: u32) -> Result<(), String> {
    let c_path = CString::new(path.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid path: {e}"))?;
    let rc = unsafe { libc::chown(c_path.as_ptr(), uid, gid) };
    if rc != 0 {
        return Err(format!(
            "chown {} to {uid}:{gid} failed: {}",
            path.display(),
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn pivot_root(new_root: &Path, put_old: &Path) -> Result<(), String> {
    let c_new = CString::new(new_root.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid new_root: {e}"))?;
    let c_old = CString::new(put_old.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid put_old: {e}"))?;
    let rc = unsafe { libc::syscall(libc::SYS_pivot_root, c_new.as_ptr(), c_old.as_ptr()) };
    if rc != 0 {
        return Err(format!(
            "SYS_pivot_root failed: {}",
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn chroot(new_root: &Path) -> Result<(), String> {
    let c_new = CString::new(new_root.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid new_root: {e}"))?;
    let rc = unsafe { libc::chroot(c_new.as_ptr()) };
    if rc != 0 {
        return Err(format!(
            "chroot failed: {}",
            std::io::Error::last_os_error()
        ));
    }
    env::set_current_dir("/").map_err(|e| format!("chdir / after chroot: {e}"))
}

fn compose_ca_bundle(
    base_bundle: Option<&Path>,
    extra_cert: &Path,
    out_path: &Path,
) -> Result<bool, String> {
    if !extra_cert.exists() {
        return Ok(false);
    }
    let extra_raw = fs::read_to_string(extra_cert)
        .map_err(|e| format!("read {}: {e}", extra_cert.display()))?;
    let extra_trimmed = extra_raw.trim();
    if extra_trimmed.is_empty() {
        return Ok(false);
    }

    let mut combined = String::new();
    if let Some(base) = base_bundle {
        if base.exists() {
            let base_raw =
                fs::read_to_string(base).map_err(|e| format!("read {}: {e}", base.display()))?;
            combined.push_str(&base_raw);
            if !combined.is_empty() && !combined.ends_with('\n') {
                combined.push('\n');
            }
        }
    }
    combined.push_str(extra_trimmed);
    combined.push('\n');

    if let Some(parent) = out_path.parent() {
        fs::create_dir_all(parent).map_err(|e| format!("mkdir {}: {e}", parent.display()))?;
    }
    fs::write(out_path, combined.as_bytes())
        .map_err(|e| format!("write {}: {e}", out_path.display()))?;
    fs::set_permissions(out_path, fs::Permissions::from_mode(0o644))
        .map_err(|e| format!("chmod {}: {e}", out_path.display()))?;
    Ok(true)
}

fn unmount_lazy(target: &Path) -> Result<(), String> {
    let c_target = CString::new(target.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid unmount path: {e}"))?;
    let rc = unsafe { libc::umount2(c_target.as_ptr(), libc::MNT_DETACH) };
    if rc != 0 {
        return Err(format!(
            "umount2 MNT_DETACH {} failed: {}",
            target.display(),
            std::io::Error::last_os_error()
        ));
    }
    Ok(())
}

fn unmount_lazy_ignore_missing(target: &Path) {
    let Ok(c_target) = CString::new(target.to_string_lossy().as_bytes()) else {
        return;
    };
    let rc = unsafe { libc::umount2(c_target.as_ptr(), libc::MNT_DETACH) };
    if rc != 0 {
        let err = std::io::Error::last_os_error();
        if !matches!(err.raw_os_error(), Some(libc::ENOENT) | Some(libc::EINVAL)) {
            eprintln!(
                "[podd-init] warning: umount2 MNT_DETACH {} failed: {err}",
                target.display()
            );
        }
    }
}

const E2B_MOUNTS_ENV: &str = "CUBEPOD_E2B_MOUNTS";
const E2B_MOUNTS_FILE: &str = "/etc/cubepod/e2b-mounts.json";
const E2B_RESERVED_PATHS: &[&str] = &[
    "/rootfs-upper",
    "/opt/cubepod",
    "/mnt/cubepod",
    "/proc",
    "/sys",
    "/dev",
    "/run",
    "/etc",
    "/bin",
    "/sbin",
    "/usr",
    "/lib",
    "/lib64",
    "/root",
    "/boot",
];

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct E2BMountSpec {
    pub volume_id: String,
    pub staging_path: String,
    pub target_path: String,
}

fn load_e2b_mounts(workspace_dir: &str) -> Result<Vec<E2BMountSpec>, String> {
    let env_val = env::var(E2B_MOUNTS_ENV).ok();
    load_e2b_mounts_from(
        env_val.as_deref(),
        Path::new(E2B_MOUNTS_FILE),
        workspace_dir,
    )
}

fn load_e2b_mounts_from(
    env_val: Option<&str>,
    file_path: &Path,
    workspace_dir: &str,
) -> Result<Vec<E2BMountSpec>, String> {
    if let Some(raw) = env_val {
        if !raw.trim().is_empty() {
            return parse_and_validate_e2b_mounts(raw, workspace_dir);
        }
    }
    if file_path.exists() {
        let raw = fs::read_to_string(file_path)
            .map_err(|e| format!("failed to read {}: {e}", file_path.display()))?;
        if raw.trim().is_empty() {
            return Ok(Vec::new());
        }
        return parse_and_validate_e2b_mounts(&raw, workspace_dir);
    }
    Ok(Vec::new())
}

pub fn parse_and_validate_e2b_mounts(
    raw: &str,
    workspace_dir: &str,
) -> Result<Vec<E2BMountSpec>, String> {
    let items = JsonParser::new(raw).parse_root_array()?;
    let norm_workspace = normalize_abs_path(workspace_dir, "workspace_dir")?;

    let mut seen_targets = HashSet::new();
    let mut specs: Vec<E2BMountSpec> = Vec::with_capacity(items.len());

    for (idx, item) in items.into_iter().enumerate() {
        let JsonValue::Object(fields) = item else {
            return Err(format!("e2b mount entry {idx} must be a JSON object"));
        };

        let mut volume_id: Option<String> = None;
        let mut staging_path: Option<String> = None;
        let mut target_path: Option<String> = None;

        for (k, v) in fields {
            match k.as_str() {
                "volume_id" => {
                    volume_id = Some(expect_json_string(v, idx, "volume_id")?);
                }
                "staging_path" => {
                    staging_path = Some(expect_json_string(v, idx, "staging_path")?);
                }
                "target_path" => {
                    target_path = Some(expect_json_string(v, idx, "target_path")?);
                }
                _ => {}
            }
        }

        let volume_id =
            volume_id.ok_or_else(|| format!("e2b mount entry {idx} missing volume_id"))?;
        let staging_path =
            staging_path.ok_or_else(|| format!("e2b mount entry {idx} missing staging_path"))?;
        let target_path =
            target_path.ok_or_else(|| format!("e2b mount entry {idx} missing target_path"))?;

        if !is_valid_volume_id(&volume_id) {
            return Err(format!(
                "e2b mount entry {idx} has invalid volume_id {volume_id:?}: must be 32 lowercase hex chars"
            ));
        }

        let expected_staging = format!("/run/cubepod/volumes/{volume_id}");
        if staging_path != expected_staging {
            return Err(format!(
                "e2b mount entry {idx} has invalid staging_path {staging_path:?}: expected {expected_staging:?}"
            ));
        }

        let norm_target = normalize_abs_path(&target_path, "target_path")?;

        if paths_overlap(&norm_target, &norm_workspace) {
            return Err(format!(
                "e2b mount target_path {norm_target:?} overlaps workspace_dir {norm_workspace:?}"
            ));
        }
        for reserved in E2B_RESERVED_PATHS {
            if paths_overlap(&norm_target, reserved) {
                return Err(format!(
                    "e2b mount target_path {norm_target:?} overlaps reserved path {reserved:?}"
                ));
            }
        }
        for prev in &specs {
            if prev.target_path == norm_target {
                return Err(format!("duplicate e2b mount target_path {norm_target:?}"));
            }
            if paths_overlap(&norm_target, &prev.target_path) {
                return Err(format!(
                    "e2b mount target_path {norm_target:?} overlaps mount {:?}",
                    prev.target_path
                ));
            }
        }
        seen_targets.insert(norm_target.clone());

        specs.push(E2BMountSpec {
            volume_id,
            staging_path,
            target_path: norm_target,
        });
    }

    Ok(specs)
}

pub fn mount_e2b_volumes(
    mounts: &[E2BMountSpec],
    root: &Path,
    default_uid: u32,
    default_gid: u32,
    no_pivot: bool,
) -> Result<(), String> {
    for mount in mounts {
        let src_path = Path::new(&mount.staging_path);
        if !src_path.is_dir() {
            return Err(format!(
                "e2b volume {} staging path {} does not exist or is not a directory",
                mount.volume_id, mount.staging_path
            ));
        }

        let dst = root.join(mount.target_path.trim_start_matches('/'));
        fs::create_dir_all(&dst).map_err(|e| format!("mkdir e2b target {}: {e}", dst.display()))?;
        chown_path(&dst, default_uid, default_gid)?;

        // With no pivot, Kubernetes can mount the same PVC/subPath at both
        // locations. Do not issue mount(2) in that case: a standard container
        // intentionally has no CAP_SYS_ADMIN. Pivoted roots still use the
        // bind/remount path below, including its nosuid/nodev hardening.
        use std::os::unix::fs::MetadataExt;
        let src_meta = fs::metadata(src_path).map_err(|e| format!("stat e2b source: {e}"))?;
        let dst_meta = fs::metadata(&dst).map_err(|e| format!("stat e2b target: {e}"))?;
        if no_pivot && src_meta.dev() == dst_meta.dev() && src_meta.ino() == dst_meta.ino() {
            continue;
        }

        let c_src = CString::new(mount.staging_path.as_bytes())
            .map_err(|e| format!("invalid staging_path {}: {e}", mount.staging_path))?;
        let c_dst = CString::new(dst.to_string_lossy().as_bytes())
            .map_err(|e| format!("invalid target_path {}: {e}", dst.display()))?;

        let rc = unsafe {
            libc::mount(
                c_src.as_ptr(),
                c_dst.as_ptr(),
                std::ptr::null(),
                libc::MS_BIND as libc::c_ulong,
                std::ptr::null(),
            )
        };
        if rc != 0 {
            return Err(format!(
                "bind mount {} -> {} failed: {}",
                mount.staging_path,
                dst.display(),
                std::io::Error::last_os_error()
            ));
        }

        let remount_flags =
            (libc::MS_BIND | libc::MS_REMOUNT | libc::MS_NOSUID | libc::MS_NODEV) as libc::c_ulong;
        let rc = unsafe {
            libc::mount(
                std::ptr::null(),
                c_dst.as_ptr(),
                std::ptr::null(),
                remount_flags,
                std::ptr::null(),
            )
        };
        if rc != 0 {
            return Err(format!(
                "remount nosuid,nodev on {} failed: {}",
                dst.display(),
                std::io::Error::last_os_error()
            ));
        }
    }
    Ok(())
}

fn expect_json_string(val: JsonValue, idx: usize, field: &str) -> Result<String, String> {
    match val {
        JsonValue::String(s) => Ok(s),
        _ => Err(format!(
            "e2b mount entry {idx} field {field} must be a string"
        )),
    }
}

fn is_valid_volume_id(id: &str) -> bool {
    id.len() == 32 && id.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

fn normalize_abs_path(raw: &str, label: &str) -> Result<String, String> {
    if raw.contains('\0') {
        return Err(format!("{label} contains NUL byte"));
    }
    if !raw.starts_with('/') {
        return Err(format!("{label} {raw:?} must start with '/'"));
    }
    let mut segments = Vec::new();
    for seg in raw.split('/') {
        if seg.is_empty() {
            continue;
        }
        if seg == "." || seg == ".." {
            return Err(format!(
                "{label} {raw:?} must not contain '.' or '..' segments"
            ));
        }
        segments.push(seg);
    }
    if segments.is_empty() {
        return Err(format!("{label} must not be '/'"));
    }
    Ok(format!("/{}", segments.join("/")))
}

fn paths_overlap(a: &str, b: &str) -> bool {
    if a == b {
        return true;
    }
    let a_child_of_b = a.len() > b.len() && a.starts_with(b) && a.as_bytes()[b.len()] == b'/';
    let b_child_of_a = b.len() > a.len() && b.starts_with(a) && b.as_bytes()[a.len()] == b'/';
    a_child_of_b || b_child_of_a
}

#[derive(Debug)]
enum JsonValue {
    String(String),
    Object(Vec<(String, JsonValue)>),
    Other,
}

struct JsonParser<'a> {
    bytes: &'a [u8],
    pos: usize,
}

impl<'a> JsonParser<'a> {
    fn new(input: &'a str) -> Self {
        Self {
            bytes: input.as_bytes(),
            pos: 0,
        }
    }

    fn parse_root_array(&mut self) -> Result<Vec<JsonValue>, String> {
        self.skip_ws();
        let arr = self.parse_array()?;
        self.skip_ws();
        if self.pos != self.bytes.len() {
            return Err(format!(
                "unexpected trailing characters at byte {}",
                self.pos
            ));
        }
        Ok(arr)
    }

    fn skip_ws(&mut self) {
        while let Some(&b) = self.bytes.get(self.pos) {
            if matches!(b, b' ' | b'\t' | b'\n' | b'\r') {
                self.pos += 1;
            } else {
                break;
            }
        }
    }

    fn peek(&self) -> Option<u8> {
        self.bytes.get(self.pos).copied()
    }

    fn next(&mut self) -> Option<u8> {
        let b = self.bytes.get(self.pos).copied()?;
        self.pos += 1;
        Some(b)
    }

    fn expect(&mut self, expected: u8) -> Result<(), String> {
        match self.next() {
            Some(b) if b == expected => Ok(()),
            Some(b) => Err(format!(
                "expected '{}' at byte {}, got '{}'",
                expected as char,
                self.pos - 1,
                b as char
            )),
            None => Err(format!("unexpected EOF, expected '{}'", expected as char)),
        }
    }

    fn parse_value(&mut self) -> Result<JsonValue, String> {
        self.skip_ws();
        match self.peek() {
            Some(b'"') => Ok(JsonValue::String(self.parse_string()?)),
            Some(b'[') => {
                self.parse_array()?;
                Ok(JsonValue::Other)
            }
            Some(b'{') => Ok(JsonValue::Object(self.parse_object()?)),
            Some(b't') => self.parse_literal(b"true"),
            Some(b'f') => self.parse_literal(b"false"),
            Some(b'n') => self.parse_literal(b"null"),
            Some(b'-' | b'0'..=b'9') => self.parse_number(),
            Some(b) => Err(format!(
                "unexpected character '{}' at byte {}",
                b as char, self.pos
            )),
            None => Err("unexpected EOF while parsing JSON value".to_string()),
        }
    }

    fn parse_array(&mut self) -> Result<Vec<JsonValue>, String> {
        self.expect(b'[')?;
        let mut items = Vec::new();
        self.skip_ws();
        if self.peek() == Some(b']') {
            self.pos += 1;
            return Ok(items);
        }
        loop {
            items.push(self.parse_value()?);
            self.skip_ws();
            match self.next() {
                Some(b',') => {}
                Some(b']') => return Ok(items),
                Some(b) => {
                    return Err(format!(
                        "expected ',' or ']' at byte {}, got '{}'",
                        self.pos - 1,
                        b as char
                    ))
                }
                None => return Err("unexpected EOF in JSON array".to_string()),
            }
        }
    }

    fn parse_object(&mut self) -> Result<Vec<(String, JsonValue)>, String> {
        self.expect(b'{')?;
        let mut fields = Vec::new();
        self.skip_ws();
        if self.peek() == Some(b'}') {
            self.pos += 1;
            return Ok(fields);
        }
        loop {
            self.skip_ws();
            let key = self.parse_string()?;
            self.skip_ws();
            self.expect(b':')?;
            let val = self.parse_value()?;
            fields.push((key, val));
            self.skip_ws();
            match self.next() {
                Some(b',') => {}
                Some(b'}') => return Ok(fields),
                Some(b) => {
                    return Err(format!(
                        "expected ',' or '}}' at byte {}, got '{}'",
                        self.pos - 1,
                        b as char
                    ))
                }
                None => return Err("unexpected EOF in JSON object".to_string()),
            }
        }
    }

    fn parse_string(&mut self) -> Result<String, String> {
        self.expect(b'"')?;
        let start = self.pos;
        let mut out = String::new();
        let mut chunk_start = start;

        while let Some(b) = self.next() {
            match b {
                b'"' => {
                    let tail = std::str::from_utf8(&self.bytes[chunk_start..self.pos - 1])
                        .map_err(|e| format!("invalid UTF-8 in JSON string: {e}"))?;
                    out.push_str(tail);
                    return Ok(out);
                }
                b'\\' => {
                    let prefix = std::str::from_utf8(&self.bytes[chunk_start..self.pos - 1])
                        .map_err(|e| format!("invalid UTF-8 in JSON string: {e}"))?;
                    out.push_str(prefix);
                    let esc = self
                        .next()
                        .ok_or_else(|| "unexpected EOF in JSON string escape".to_string())?;
                    match esc {
                        b'"' => out.push('"'),
                        b'\\' => out.push('\\'),
                        b'/' => out.push('/'),
                        b'b' => out.push('\x08'),
                        b'f' => out.push('\x0c'),
                        b'n' => out.push('\n'),
                        b'r' => out.push('\r'),
                        b't' => out.push('\t'),
                        b'u' => {
                            let cp = self.parse_hex4()?;
                            let ch = char::from_u32(cp as u32)
                                .ok_or_else(|| format!("invalid unicode escape \\u{cp:04x}"))?;
                            out.push(ch);
                        }
                        other => {
                            return Err(format!("invalid JSON escape \\{}", other as char));
                        }
                    }
                    chunk_start = self.pos;
                }
                0x00..=0x1f => {
                    return Err("unescaped control character in JSON string".to_string());
                }
                _ => {}
            }
        }
        Err("unterminated JSON string".to_string())
    }

    fn parse_hex4(&mut self) -> Result<u16, String> {
        let mut val: u16 = 0;
        for _ in 0..4 {
            let b = self
                .next()
                .ok_or_else(|| "unexpected EOF in \\u escape".to_string())?;
            let digit = match b {
                b'0'..=b'9' => (b - b'0') as u16,
                b'a'..=b'f' => (b - b'a' + 10) as u16,
                b'A'..=b'F' => (b - b'A' + 10) as u16,
                _ => return Err(format!("invalid hex digit '{}' in \\u escape", b as char)),
            };
            val = (val << 4) | digit;
        }
        Ok(val)
    }

    fn parse_literal(&mut self, lit: &[u8]) -> Result<JsonValue, String> {
        if self.bytes[self.pos..].starts_with(lit) {
            self.pos += lit.len();
            Ok(JsonValue::Other)
        } else {
            Err(format!("invalid JSON literal at byte {}", self.pos))
        }
    }

    fn parse_number(&mut self) -> Result<JsonValue, String> {
        let start = self.pos;
        if self.peek() == Some(b'-') {
            self.pos += 1;
        }
        let mut has_digits = false;
        while matches!(self.peek(), Some(b'0'..=b'9')) {
            self.pos += 1;
            has_digits = true;
        }
        if !has_digits {
            return Err(format!("invalid JSON number at byte {start}"));
        }
        if self.peek() == Some(b'.') {
            self.pos += 1;
            let mut frac_digits = false;
            while matches!(self.peek(), Some(b'0'..=b'9')) {
                self.pos += 1;
                frac_digits = true;
            }
            if !frac_digits {
                return Err(format!("invalid JSON number fraction at byte {start}"));
            }
        }
        if matches!(self.peek(), Some(b'e' | b'E')) {
            self.pos += 1;
            if matches!(self.peek(), Some(b'+' | b'-')) {
                self.pos += 1;
            }
            let mut exp_digits = false;
            while matches!(self.peek(), Some(b'0'..=b'9')) {
                self.pos += 1;
                exp_digits = true;
            }
            if !exp_digits {
                return Err(format!("invalid JSON number exponent at byte {start}"));
            }
        }
        Ok(JsonValue::Other)
    }
}

fn main() -> ExitCode {
    let args: Vec<String> = env::args().skip(1).collect();
    let cfg = InitConfig::from_env_and_args(args);

    eprintln!(
        "[podd-init] starting CubePod PID 1 Rust RootFS Mounter (pid={}, rootfs_dev={}, ws_dev={}, next={:?})",
        std::process::id(),
        cfg.rootfs_device.display(),
        cfg.workspace_device.display(),
        cfg.next_argv
    );

    if let Err(err) = setup_and_pivot(&cfg) {
        eprintln!("[podd-init] fatal error setting up persistent rootfs: {err}");
        return ExitCode::from(1);
    }

    let program = &cfg.next_argv[0];
    let err = Command::new(program).args(&cfg.next_argv[1..]).exec();
    eprintln!("[podd-init] failed to exec {}: {err}", program);
    ExitCode::from(1)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::{SystemTime, UNIX_EPOCH};

    static TEST_COUNTER: AtomicUsize = AtomicUsize::new(1);

    fn unique_temp_path(prefix: &str, ext: &str) -> PathBuf {
        let count = TEST_COUNTER.fetch_add(1, Ordering::SeqCst);
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        env::temp_dir().join(format!("{prefix}-{}-{count}.{ext}", nanos))
    }

    struct TempFileGuard(PathBuf);
    impl Drop for TempFileGuard {
        fn drop(&mut self) {
            let _ = fs::remove_file(&self.0);
        }
    }

    #[test]
    fn default_config_values() {
        let cfg = InitConfig::from_env_and_args(vec![]);
        assert_eq!(cfg.default_uid, 1000);
        assert_eq!(cfg.default_gid, 1000);
        assert_eq!(
            cfg.init_state_path,
            PathBuf::from("/run/cubepod/init-state.json")
        );
        assert_eq!(cfg.next_argv, vec!["/opt/cubepod/bin/podd".to_string()]);
    }

    #[test]
    fn default_next_argv_points_to_podd() {
        let cfg = InitConfig::from_env_and_args(vec![]);
        assert_eq!(cfg.next_argv, vec!["/opt/cubepod/bin/podd".to_string()]);
    }

    #[test]
    fn graceful_fallback_when_block_device_missing() {
        let state_path = unique_temp_path("test-fallback-state", "json");
        let _guard = TempFileGuard(state_path.clone());

        let mut cfg = InitConfig::from_env_and_args(vec![]);
        cfg.rootfs_device = PathBuf::from("/dev/nonexistent-cubepod-rootfs-test");
        cfg.init_state_path = state_path.clone();

        assert!(setup_and_pivot(&cfg).is_ok());

        assert!(state_path.exists());
        let content = fs::read_to_string(&state_path).expect("read init-state.json");
        assert!(content.contains("\"pivoted\": false"));
        assert!(content.contains("\"rootfs_action\": \"skipped\""));
        assert!(content.contains("\"workspace_action\": \"skipped\""));
        assert!(content.contains("\"duration_ms\":"));
        assert!(content.contains("\"e2b_bind_duration_ms\":"));
    }

    #[test]
    fn skip_pivot_writes_skipped_state() {
        let state_path = unique_temp_path("test-skip-state", "json");
        let _guard = TempFileGuard(state_path.clone());

        let mut cfg = InitConfig::from_env_and_args(vec![]);
        cfg.skip_pivot = true;
        cfg.init_state_path = state_path.clone();

        assert!(setup_and_pivot(&cfg).is_ok());

        assert!(state_path.exists());
        let content = fs::read_to_string(&state_path).expect("read init-state.json");
        assert!(content.contains("\"pivoted\": false"));
        assert!(content.contains("\"rootfs_action\": \"skipped\""));
        assert!(content.contains("\"workspace_action\": \"skipped\""));
    }

    #[test]
    fn sparse_file_ensure_ext4_and_init_state_verification() {
        let img_path = unique_temp_path("test-sparse", "img");
        let _guard_img = TempFileGuard(img_path.clone());

        // Create 64MB sparse file
        let file = fs::File::create(&img_path).expect("create sparse file");
        file.set_len(64 * 1024 * 1024).expect("truncate to 64MB");
        drop(file);

        // Run 1: format raw image with ext4
        let action1 = ensure_ext4(&img_path, "cubepod-test").expect("ensure_ext4 run 1");
        assert_eq!(
            action1,
            VolumeAction::Formatted,
            "first ensure_ext4 run must format fresh sparse image"
        );

        // Run 2: existing ext4 image verified/healed with e2fsck
        let action2 = ensure_ext4(&img_path, "cubepod-test").expect("ensure_ext4 run 2");
        assert!(
            action2 == VolumeAction::Mounted || action2 == VolumeAction::Healed,
            "second ensure_ext4 run must return Mounted or Healed, got {:?}",
            action2
        );

        // Verify init-state.json output serialization and file writing
        let state_path = unique_temp_path("test-init-state", "json");
        let _guard_state = TempFileGuard(state_path.clone());

        let state = InitState {
            pivoted: true,
            rootfs_device: img_path.to_string_lossy().to_string(),
            rootfs_action: action2,
            workspace_device: "/dev/cubepod-workspace".to_string(),
            workspace_action: VolumeAction::Skipped,
            duration_ms: 128,
            e2b_bind_duration_ms: 5,
        };

        state.write_to_file(&state_path).expect("write init state");
        assert!(state_path.exists());

        let content = fs::read_to_string(&state_path).expect("read init-state.json");
        assert!(content.contains("\"pivoted\": true"));
        assert!(content.contains(&format!("\"rootfs_device\": \"{}\"", img_path.display())));
        assert!(content.contains(&format!("\"rootfs_action\": \"{}\"", action2.as_str())));
        assert!(content.contains("\"workspace_device\": \"/dev/cubepod-workspace\""));
        assert!(content.contains("\"workspace_action\": \"skipped\""));
        assert!(content.contains("\"duration_ms\": 128"));
        assert!(content.contains("\"e2b_bind_duration_ms\": 5"));
    }

    #[test]
    fn volume_action_as_str_and_display() {
        assert_eq!(VolumeAction::Formatted.as_str(), "formatted");
        assert_eq!(VolumeAction::Healed.as_str(), "healed");
        assert_eq!(VolumeAction::Mounted.as_str(), "mounted");
        assert_eq!(VolumeAction::Skipped.as_str(), "skipped");

        assert_eq!(format!("{}", VolumeAction::Formatted), "formatted");
        assert_eq!(format!("{}", VolumeAction::Healed), "healed");
        assert_eq!(format!("{}", VolumeAction::Mounted), "mounted");
        assert_eq!(format!("{}", VolumeAction::Skipped), "skipped");
    }

    #[test]
    fn init_state_to_json_format() {
        let state = InitState {
            pivoted: true,
            rootfs_device: "/dev/cubepod-rootfs".to_string(),
            rootfs_action: VolumeAction::Mounted,
            workspace_device: "/dev/cubepod-workspace".to_string(),
            workspace_action: VolumeAction::Mounted,
            duration_ms: 42,
            e2b_bind_duration_ms: 7,
        };

        let json = state.to_json();
        let expected = "{\n  \"pivoted\": true,\n  \"rootfs_device\": \"/dev/cubepod-rootfs\",\n  \"rootfs_action\": \"mounted\",\n  \"workspace_device\": \"/dev/cubepod-workspace\",\n  \"workspace_action\": \"mounted\",\n  \"duration_ms\": 42,\n  \"e2b_bind_duration_ms\": 7\n}\n";
        assert_eq!(json, expected);
    }

    #[test]
    fn compose_ca_bundle_appends_extra_cert_with_0644_mode() {
        let base_path = unique_temp_path("test-ca-base", "crt");
        let extra_path = unique_temp_path("test-ca-extra", "crt");
        let out_path = unique_temp_path("test-ca-out", "crt");
        let _g1 = TempFileGuard(base_path.clone());
        let _g2 = TempFileGuard(extra_path.clone());
        let _g3 = TempFileGuard(out_path.clone());

        fs::write(&base_path, "BASE_CERT_DATA").expect("write base");
        fs::write(&extra_path, "  EXTRA_MITM_CERT  \n").expect("write extra");

        let wrote =
            compose_ca_bundle(Some(&base_path), &extra_path, &out_path).expect("compose_ca_bundle");
        assert!(wrote);

        let content = fs::read_to_string(&out_path).expect("read composed bundle");
        assert_eq!(content, "BASE_CERT_DATA\nEXTRA_MITM_CERT\n");
        let mode = fs::metadata(&out_path)
            .expect("metadata")
            .permissions()
            .mode()
            & 0o777;
        assert_eq!(mode, 0o644);

        let missing_extra = unique_temp_path("test-ca-missing", "crt");
        let skipped = compose_ca_bundle(Some(&base_path), &missing_extra, &out_path)
            .expect("missing extra should skip cleanly");
        assert!(!skipped);
    }

    #[test]
    fn e2b_mounts_valid_parsing_and_normalization() {
        let vid1 = "0123456789abcdef0123456789abcdef";
        let vid2 = "fedcba9876543210fedcba9876543210";
        let raw = format!(
            r#"[
                {{
                    "volume_id": "{vid1}",
                    "staging_path": "/run/cubepod/volumes/{vid1}",
                    "target_path": "/mnt/dataset"
                }},
                {{
                    "target_path": "//mnt//models/",
                    "staging_path": "/run/cubepod/volumes/{vid2}",
                    "volume_id": "{vid2}"
                }}
            ]"#
        );

        let specs = parse_and_validate_e2b_mounts(&raw, "/workspace").expect("valid mounts");
        assert_eq!(
            specs,
            vec![
                E2BMountSpec {
                    volume_id: vid1.to_string(),
                    staging_path: format!("/run/cubepod/volumes/{vid1}"),
                    target_path: "/mnt/dataset".to_string(),
                },
                E2BMountSpec {
                    volume_id: vid2.to_string(),
                    staging_path: format!("/run/cubepod/volumes/{vid2}"),
                    target_path: "/mnt/models".to_string(),
                },
            ]
        );

        // Empty array is valid
        assert!(parse_and_validate_e2b_mounts("[]", "/workspace")
            .unwrap()
            .is_empty());

        // Sibling prefix like /workspace2 does not overlap /workspace
        let sibling = format!(
            r#"[{{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/workspace2"}}]"#
        );
        assert_eq!(
            parse_and_validate_e2b_mounts(&sibling, "/workspace")
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn e2b_mounts_rejects_invalid_volume_id() {
        let cases = [
            // Missing volume_id
            r#"[{"staging_path":"/run/cubepod/volumes/0123456789abcdef0123456789abcdef","target_path":"/mnt/data"}]"#,
            // Too short
            r#"[{"volume_id":"0123456789abcdef","staging_path":"/run/cubepod/volumes/0123456789abcdef","target_path":"/mnt/data"}]"#,
            // Uppercase hex
            r#"[{"volume_id":"0123456789ABCDEF0123456789abcdef","staging_path":"/run/cubepod/volumes/0123456789ABCDEF0123456789abcdef","target_path":"/mnt/data"}]"#,
            // Non-hex char 'g'
            r#"[{"volume_id":"0123456789abcdeg0123456789abcdef","staging_path":"/run/cubepod/volumes/0123456789abcdeg0123456789abcdef","target_path":"/mnt/data"}]"#,
        ];

        for raw in cases {
            assert!(
                parse_and_validate_e2b_mounts(raw, "/workspace").is_err(),
                "expected error for {raw}"
            );
        }
    }

    #[test]
    fn e2b_mounts_rejects_bad_staging_path() {
        let vid = "0123456789abcdef0123456789abcdef";
        let other_vid = "ffffffffffffffffffffffffffffffff";
        let cases = [
            format!(
                r#"[{{"volume_id":"{vid}","staging_path":"/tmp/volumes/{vid}","target_path":"/mnt/data"}}]"#
            ),
            format!(
                r#"[{{"volume_id":"{vid}","staging_path":"/run/cubepod/volumes/{other_vid}","target_path":"/mnt/data"}}]"#
            ),
            format!(
                r#"[{{"volume_id":"{vid}","staging_path":"/run/cubepod/volumes/{vid}/extra","target_path":"/mnt/data"}}]"#
            ),
        ];

        for raw in cases {
            assert!(
                parse_and_validate_e2b_mounts(&raw, "/workspace").is_err(),
                "expected error for {raw}"
            );
        }
    }

    #[test]
    fn e2b_mounts_rejects_reserved_workspace_traversal_and_duplicates() {
        let vid1 = "0123456789abcdef0123456789abcdef";
        let vid2 = "fedcba9876543210fedcba9876543210";

        let bad_targets = [
            "/workspace",
            "/workspace/sub",
            "/rootfs-upper",
            "/rootfs-upper/data",
            "/etc",
            "/etc/ssl",
            "/mnt",
            "/opt",
            "/",
            "mnt/data",
            "/mnt/../etc",
            "/mnt/./data",
            "/..",
        ];

        for target in bad_targets {
            let raw = format!(
                r#"[{{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"{target}"}}]"#
            );
            assert!(
                parse_and_validate_e2b_mounts(&raw, "/workspace").is_err(),
                "expected target_path {target:?} to be rejected"
            );
        }

        // Duplicate target_path (including after consecutive-slash normalization)
        let dup_raw = format!(
            r#"[
                {{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/mnt/data"}},
                {{"volume_id":"{vid2}","staging_path":"/run/cubepod/volumes/{vid2}","target_path":"//mnt//data/"}}
            ]"#
        );
        let err = parse_and_validate_e2b_mounts(&dup_raw, "/workspace").unwrap_err();
        assert!(err.contains("duplicate"), "unexpected error: {err}");

        // Overlapping nested target_path between two E2B mounts
        let nested_raw = format!(
            r#"[
                {{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/mnt/ds"}},
                {{"volume_id":"{vid2}","staging_path":"/run/cubepod/volumes/{vid2}","target_path":"/mnt/ds/sub"}}
            ]"#
        );
        let err = parse_and_validate_e2b_mounts(&nested_raw, "/workspace").unwrap_err();
        assert!(err.contains("overlaps mount"), "unexpected error: {err}");
    }

    #[test]
    fn e2b_mounts_env_and_file_fallback() {
        let vid1 = "0123456789abcdef0123456789abcdef";
        let vid2 = "fedcba9876543210fedcba9876543210";
        let file_path = unique_temp_path("test-e2b-mounts", "json");
        let _guard = TempFileGuard(file_path.clone());

        // Missing file and no env -> empty
        assert!(load_e2b_mounts_from(None, &file_path, "/workspace")
            .unwrap()
            .is_empty());

        // File fallback when env is None or empty
        let file_json = format!(
            r#"[{{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/mnt/from-file"}}]"#
        );
        fs::write(&file_path, file_json).expect("write e2b-mounts.json");

        let from_file = load_e2b_mounts_from(Some(""), &file_path, "/workspace").unwrap();
        assert_eq!(from_file.len(), 1);
        assert_eq!(from_file[0].target_path, "/mnt/from-file");

        // Non-empty env overrides file
        let env_json = format!(
            r#"[{{"volume_id":"{vid2}","staging_path":"/run/cubepod/volumes/{vid2}","target_path":"/mnt/from-env"}}]"#
        );
        let from_env = load_e2b_mounts_from(Some(&env_json), &file_path, "/workspace").unwrap();
        assert_eq!(from_env.len(), 1);
        assert_eq!(from_env[0].target_path, "/mnt/from-env");
    }

    #[test]
    fn mount_e2b_volumes_fails_fast_when_staging_missing() {
        let vid = "0123456789abcdef0123456789abcdef";
        let spec = E2BMountSpec {
            volume_id: vid.to_string(),
            staging_path: format!("/run/cubepod/volumes/{vid}-nonexistent"),
            target_path: "/mnt/dataset".to_string(),
        };
        let err = mount_e2b_volumes(&[spec], Path::new("/"), 1000, 1000, false).unwrap_err();
        assert!(err.contains("does not exist"), "unexpected error: {err}");
    }
    #[test]
    fn e2b_already_mounted_target_needs_no_mount_capability() {
        let root = unique_temp_path("e2b-direct", "dir");
        let target = root.join("data");
        fs::create_dir_all(&target).unwrap();
        fs::write(target.join("persist.txt"), "real-volume").unwrap();
        let mount = E2BMountSpec {
            volume_id: "0123456789abcdef0123456789abcdef".into(),
            staging_path: target.to_string_lossy().into_owned(),
            target_path: "/data".into(),
        };
        mount_e2b_volumes(
            &[mount],
            &root,
            unsafe { libc::getuid() },
            unsafe { libc::getgid() },
            true,
        )
        .expect("an identical source/target must not require mount(2)");
        assert_eq!(
            fs::read_to_string(target.join("persist.txt")).unwrap(),
            "real-volume"
        );
        fs::remove_dir_all(root).unwrap();
    }
}
