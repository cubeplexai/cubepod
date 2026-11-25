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
use std::thread;
use std::time::{Duration, Instant};

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
    pub volume_bind_duration_ms: u64,
}

impl InitState {
    pub fn to_json(&self) -> String {
        format!(
            "{{\n  \"pivoted\": {},\n  \"rootfs_device\": \"{}\",\n  \"rootfs_action\": \"{}\",\n  \"workspace_device\": \"{}\",\n  \"workspace_action\": \"{}\",\n  \"duration_ms\": {},\n  \"volume_bind_duration_ms\": {}\n}}\n",
            self.pivoted,
            self.rootfs_device,
            self.rootfs_action.as_str(),
            self.workspace_device,
            self.workspace_action.as_str(),
            self.duration_ms,
            self.volume_bind_duration_ms
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

const MKFS_EXT4_EXTENDED_OPTS: &str = "lazy_itable_init=1,lazy_journal_init=1,nodiscard";

fn mkfs_ext4_args(label: &str) -> [&str; 7] {
    ["-F", "-L", label, "-m", "0", "-E", MKFS_EXT4_EXTENDED_OPTS]
}

fn is_blank_or_unformatted(device: &Path) -> Result<bool, String> {
    let blkid = Command::new("blkid")
        .args(["-o", "value", "-s", "TYPE"])
        .arg(device)
        .output()
        .map_err(|e| format!("failed to run blkid on {}: {e}", device.display()))?;

    let fstype = String::from_utf8_lossy(&blkid.stdout).trim().to_string();
    Ok(!blkid.status.success() || fstype.is_empty())
}

fn format_ext4(device: &Path, label: &str) -> Result<(), String> {
    eprintln!(
        "[podd-init] formatting fresh raw block volume {} as ext4 (label={})",
        device.display(),
        label
    );
    let status = Command::new("mkfs.ext4")
        .args(mkfs_ext4_args(label))
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
    Ok(())
}

fn e2fsck_heal(device: &Path) -> Result<VolumeAction, String> {
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

/// Ensures `device` has a valid `ext4` filesystem; formats with `mkfs.ext4` if blank,
/// or runs `e2fsck -p` to auto-heal after unclean VM shutdowns.
pub fn ensure_ext4(device: &Path, label: &str) -> Result<VolumeAction, String> {
    if !device.exists() {
        return Err(format!("block device {} does not exist", device.display()));
    }

    if is_blank_or_unformatted(device)? {
        format_ext4(device, label)?;
        return Ok(VolumeAction::Formatted);
    }

    e2fsck_heal(device)
}

/// Ensures `device` has a valid `ext4` filesystem and mounts it at `target`.
///
/// For existing ext4 volumes (wake/resume), attempts a direct `mount(2)` first so the
/// kernel can replay the journal without spawning `e2fsck -p`. Falls back to `e2fsck -p`
/// and retries `mount(2)` only if direct mounting fails or mounts read-only due to errors.
pub fn ensure_and_mount_ext4(
    device: &Path,
    target: &Path,
    label: &str,
) -> Result<VolumeAction, String> {
    ensure_and_mount_ext4_with(device, target, label, mount_ext4_rw, e2fsck_heal)
}

fn ensure_and_mount_ext4_with<M, C>(
    device: &Path,
    target: &Path,
    label: &str,
    mut mount_fn: M,
    mut fsck_fn: C,
) -> Result<VolumeAction, String>
where
    M: FnMut(&Path, &Path) -> Result<(), String>,
    C: FnMut(&Path) -> Result<VolumeAction, String>,
{
    if !device.exists() {
        return Err(format!("block device {} does not exist", device.display()));
    }

    if is_blank_or_unformatted(device)? {
        format_ext4(device, label)?;
        mount_fn(device, target)?;
        return Ok(VolumeAction::Formatted);
    }

    match mount_fn(device, target) {
        Ok(()) => Ok(VolumeAction::Mounted),
        Err(mount_err) => {
            eprintln!(
                "[podd-init] direct mount of {} failed ({mount_err}); falling back to e2fsck -p",
                device.display()
            );
            let action = fsck_fn(device)?;
            mount_fn(device, target)?;
            Ok(action)
        }
    }
}

pub fn setup_and_pivot(cfg: &InitConfig) -> Result<(), String> {
    let start = Instant::now();
    let ws_str = cfg.workspace_dir.to_string_lossy();
    let volume_mounts = load_volume_mounts(&ws_str)?;

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
        let mut volume_bind_duration_ms = 0u64;
        if !volume_mounts.is_empty() {
            let bind_start = Instant::now();
            mount_volumes(
                &volume_mounts,
                Path::new("/"),
                cfg.default_uid,
                cfg.default_gid,
                true,
            )?;
            volume_bind_duration_ms = bind_start.elapsed().as_millis() as u64;
        }
        let state = InitState {
            pivoted: false,
            rootfs_device: cfg.rootfs_device.to_string_lossy().to_string(),
            rootfs_action: VolumeAction::Skipped,
            workspace_device: cfg.workspace_device.to_string_lossy().to_string(),
            workspace_action: VolumeAction::Skipped,
            duration_ms: start.elapsed().as_millis() as u64,
            volume_bind_duration_ms,
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

    // 1 & 2. Mount RootFS Upper block device (direct mount on wake, format or e2fsck fallback)
    // and create upper/ and work/ directories.
    fs::create_dir_all(&cfg.upper_mount_path)
        .map_err(|e| format!("mkdir {}: {e}", cfg.upper_mount_path.display()))?;
    let rootfs_action =
        ensure_and_mount_ext4(&cfg.rootfs_device, &cfg.upper_mount_path, "cubepod-rootfs")?;

    let upper_dir = cfg.upper_mount_path.join("upper");
    let work_dir = cfg.upper_mount_path.join("work");
    for d in [&upper_dir, &work_dir, &cfg.merged_root_path] {
        fs::create_dir_all(d).map_err(|e| format!("mkdir {}: {e}", d.display()))?;
    }

    // 3. Assemble OverlayFS combining Base Image (/) as lowerdir and block PVC as upperdir.
    mount_overlay(Path::new("/"), &upper_dir, &work_dir, &cfg.merged_root_path)?;

    // 4. Mount Workspace block device at <merged>/workspace and chown to default_uid:default_gid.
    let rel_ws = cfg
        .workspace_dir
        .strip_prefix("/")
        .unwrap_or(&cfg.workspace_dir);
    let merged_workspace = cfg.merged_root_path.join(rel_ws);
    fs::create_dir_all(&merged_workspace)
        .map_err(|e| format!("mkdir {}: {e}", merged_workspace.display()))?;

    let workspace_action = if cfg.workspace_device.exists() {
        ensure_and_mount_ext4(&cfg.workspace_device, &merged_workspace, "cubepod-ws")?
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
            if let Err(err) = mount_bind(&src, &dst, true) {
                eprintln!("[podd-init] warning: {err}");
            }
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
            if let Err(err) = mount_bind(&src, &dst, false) {
                eprintln!("[podd-init] warning: {err}");
            }
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
            if let Err(err) = mount_bind(composed_ca, &ca_bundle_dst, false) {
                eprintln!("[podd-init] warning: {err}");
            }
        }
        Ok(false) => {}
        Err(err) => {
            eprintln!("[podd-init] warning: failed to compose CA bundle: {err}");
        }
    }

    let mut volume_bind_duration_ms = 0u64;
    if !volume_mounts.is_empty() {
        let bind_start = Instant::now();
        mount_volumes(
            &volume_mounts,
            &cfg.merged_root_path,
            cfg.default_uid,
            cfg.default_gid,
            false,
        )?;
        for m in &volume_mounts {
            unmount_lazy_ignore_missing(
                &cfg.merged_root_path
                    .join("run/cubepod/volumes")
                    .join(&m.volume_id),
            );
        }
        volume_bind_duration_ms = bind_start.elapsed().as_millis() as u64;
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
        volume_bind_duration_ms,
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

fn path_to_cstring(path: &Path, label: &str) -> Result<CString, String> {
    use std::os::unix::ffi::OsStrExt;
    CString::new(path.as_os_str().as_bytes())
        .map_err(|e| format!("invalid {label} path {}: {e}", path.display()))
}

fn sys_mount(
    source: Option<&CString>,
    target: &Path,
    fstype: Option<&CString>,
    flags: libc::c_ulong,
    data: Option<&CString>,
) -> Result<(), String> {
    let c_target = path_to_cstring(target, "mount target")?;
    let src_ptr = source.map_or(std::ptr::null(), |s| s.as_ptr());
    let fs_ptr = fstype.map_or(std::ptr::null(), |f| f.as_ptr());
    let data_ptr = data.map_or(std::ptr::null(), |d| d.as_ptr().cast::<libc::c_void>());
    let rc = unsafe { libc::mount(src_ptr, c_target.as_ptr(), fs_ptr, flags, data_ptr) };
    if rc != 0 {
        return Err(std::io::Error::last_os_error().to_string());
    }
    Ok(())
}

fn mount_ext4(device: &Path, target: &Path) -> Result<(), String> {
    let c_dev = path_to_cstring(device, "device")?;
    let c_fs = CString::new("ext4").map_err(|e| format!("invalid fstype: {e}"))?;
    sys_mount(
        Some(&c_dev),
        target,
        Some(&c_fs),
        libc::MS_NOATIME as libc::c_ulong,
        None,
    )
    .map_err(|e| {
        format!(
            "mount ext4 {} -> {} failed: {e}",
            device.display(),
            target.display()
        )
    })
}

fn is_mounted_readonly(target: &Path) -> Result<bool, String> {
    let c_target = path_to_cstring(target, "statvfs target")?;
    let mut stat: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statvfs(c_target.as_ptr(), &mut stat) };
    if rc != 0 {
        return Err(format!(
            "statvfs {} failed: {}",
            target.display(),
            std::io::Error::last_os_error()
        ));
    }
    Ok((stat.f_flag & libc::ST_RDONLY) != 0)
}

fn mount_ext4_rw(device: &Path, target: &Path) -> Result<(), String> {
    mount_ext4(device, target)?;
    match is_mounted_readonly(target) {
        Ok(true) => {
            if let Ok(c_target) = path_to_cstring(target, "umount target") {
                let rc = unsafe { libc::umount2(c_target.as_ptr(), 0) };
                if rc != 0 {
                    let _ = unmount_lazy(target);
                }
            }
            Err(format!(
                "ext4 {} mounted read-only at {} (filesystem errors detected)",
                device.display(),
                target.display()
            ))
        }
        Ok(false) => Ok(()),
        Err(err) => {
            eprintln!("[podd-init] warning: {err}");
            Ok(())
        }
    }
}

fn overlay_mount_options(lower: &Path, upper: &Path, work: &Path) -> String {
    format!(
        "lowerdir={},upperdir={},workdir={}",
        lower.display(),
        upper.display(),
        work.display()
    )
}

fn mount_overlay(lower: &Path, upper: &Path, work: &Path, target: &Path) -> Result<(), String> {
    let overlay_opts = overlay_mount_options(lower, upper, work);
    eprintln!(
        "[podd-init] mounting persistent OverlayFS at {} ({})",
        target.display(),
        overlay_opts
    );
    let c_src = CString::new("overlay").map_err(|e| format!("invalid overlay source: {e}"))?;
    let c_fs = CString::new("overlay").map_err(|e| format!("invalid overlay fstype: {e}"))?;
    let c_opts = CString::new(overlay_opts).map_err(|e| format!("invalid overlay options: {e}"))?;
    sys_mount(Some(&c_src), target, Some(&c_fs), 0, Some(&c_opts))
        .map_err(|e| format!("overlayfs mount at {} failed: {e}", target.display()))
}

fn mount_bind(src: &Path, dst: &Path, recursive: bool) -> Result<(), String> {
    let c_src = path_to_cstring(src, "bind source")?;
    let flags = if recursive {
        (libc::MS_BIND | libc::MS_REC) as libc::c_ulong
    } else {
        libc::MS_BIND as libc::c_ulong
    };
    sys_mount(Some(&c_src), dst, None, flags, None).map_err(|e| {
        format!(
            "{} {} -> {} failed: {e}",
            if recursive { "rbind" } else { "bind mount" },
            src.display(),
            dst.display()
        )
    })
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

const VOLUME_MOUNTS_ENV: &str = "CUBEPOD_VOLUME_MOUNTS";
const VOLUME_MOUNTS_FILE: &str = "/etc/cubepod/volume-mounts.json";
const VOLUME_RESERVED_PATHS: &[&str] = &[
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

#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum VolumeTransport {
    #[default]
    Bind,
    Nfs,
}

#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct VolumeMountSpec {
    pub volume_id: String,
    pub staging_path: String,
    pub target_path: String,
    pub transport: VolumeTransport,
    pub nfs_target: String,
}

const NFS_MOUNT_DEADLINE: Duration = Duration::from_secs(30);
const NFS_MOUNT_RETRY: Duration = Duration::from_millis(250);
const NFS_FSTYPE: &str = "nfs";
const NFS_DEFAULT_PORT: u16 = 2049;

/// Kernel NFSv3 text options for libc::mount(2). Do not pass fg/bg/retry=.
pub fn nfs_mount_data(addr: &str, port: u16) -> String {
    format!(
        "addr={addr},vers=3,proto=tcp,port={port},mountproto=tcp,mountport={port},hard,sync,rsize=1048576,wsize=1048576,nolock,noacl,noac,lookupcache=none"
    )
}

/// Parse `<clusterIP>:/<leaseID>/<volumeID>` (IPv4 or bracketed IPv6).
pub fn parse_nfs_target(target: &str, volume_id: &str) -> Result<(String, String), String> {
    let target = target.trim();
    let (addr, rest) = if let Some(rest) = target.strip_prefix('[') {
        let close = rest.find("]:/").ok_or_else(|| {
            format!("invalid nfs_target {target:?}: expected [ipv6]:/<leaseID>/<volumeID>")
        })?;
        let addr = rest[..close].to_string();
        let path = &rest[close + 3..];
        (addr, path)
    } else {
        let Some((addr, path)) = target.split_once(":/") else {
            return Err(format!(
                "invalid nfs_target {target:?}: expected <clusterIP>:/<leaseID>/<volumeID>"
            ));
        };
        if addr.is_empty() || addr.contains('/') {
            return Err(format!("invalid nfs_target {target:?}: missing cluster IP"));
        }
        (addr.to_string(), path)
    };
    let mut parts = rest.split('/');
    let lease = parts.next().unwrap_or("");
    let vol = parts.next().unwrap_or("");
    if lease.is_empty()
        || vol.is_empty()
        || parts.next().is_some()
        || !is_hex_len(lease, 32)
        || vol != volume_id
    {
        return Err(format!(
            "invalid nfs_target {target:?}: expected /<leaseID>/{volume_id}"
        ));
    }
    if addr.parse::<std::net::IpAddr>().is_err() {
        return Err(format!(
            "invalid nfs_target {target:?}: addr must be an IP literal, not {addr:?}"
        ));
    }
    Ok((addr, lease.to_string()))
}

fn is_hex_len(s: &str, n: usize) -> bool {
    s.len() == n && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

fn load_volume_mounts(workspace_dir: &str) -> Result<Vec<VolumeMountSpec>, String> {
    let env_val = env::var(VOLUME_MOUNTS_ENV).ok();
    load_volume_mounts_from(
        env_val.as_deref(),
        Path::new(VOLUME_MOUNTS_FILE),
        workspace_dir,
    )
}

fn load_volume_mounts_from(
    env_val: Option<&str>,
    file_path: &Path,
    workspace_dir: &str,
) -> Result<Vec<VolumeMountSpec>, String> {
    if let Some(raw) = env_val {
        if !raw.trim().is_empty() {
            return parse_and_validate_volume_mounts(raw, workspace_dir);
        }
    }
    if file_path.exists() {
        let raw = fs::read_to_string(file_path)
            .map_err(|e| format!("failed to read {}: {e}", file_path.display()))?;
        if raw.trim().is_empty() {
            return Ok(Vec::new());
        }
        return parse_and_validate_volume_mounts(&raw, workspace_dir);
    }
    Ok(Vec::new())
}

pub fn parse_and_validate_volume_mounts(
    raw: &str,
    workspace_dir: &str,
) -> Result<Vec<VolumeMountSpec>, String> {
    let items = JsonParser::new(raw).parse_root_array()?;
    let norm_workspace = normalize_abs_path(workspace_dir, "workspace_dir")?;

    let mut seen_targets = HashSet::new();
    let mut specs: Vec<VolumeMountSpec> = Vec::with_capacity(items.len());

    for (idx, item) in items.into_iter().enumerate() {
        let JsonValue::Object(fields) = item else {
            return Err(format!("volume mount entry {idx} must be a JSON object"));
        };

        let mut volume_id: Option<String> = None;
        let mut staging_path: Option<String> = None;
        let mut target_path: Option<String> = None;
        let mut transport: Option<String> = None;
        let mut nfs_target: Option<String> = None;

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
                "transport" => {
                    transport = Some(expect_json_string(v, idx, "transport")?);
                }
                "nfs_target" => {
                    nfs_target = Some(expect_json_string(v, idx, "nfs_target")?);
                }
                _ => {}
            }
        }

        let volume_id =
            volume_id.ok_or_else(|| format!("volume mount entry {idx} missing volume_id"))?;
        let target_path =
            target_path.ok_or_else(|| format!("volume mount entry {idx} missing target_path"))?;

        if !is_valid_volume_id(&volume_id) {
            return Err(format!(
                "volume mount entry {idx} has invalid volume_id {volume_id:?}: must be 32 lowercase hex chars"
            ));
        }

        let transport = match transport.as_deref().unwrap_or("").trim() {
            "" | "bind" => VolumeTransport::Bind,
            "nfs" => VolumeTransport::Nfs,
            other => {
                return Err(format!(
                    "volume mount entry {idx} has unknown transport {other:?}"
                ));
            }
        };

        let (staging_path, nfs_target) = match transport {
            VolumeTransport::Nfs => {
                let target = nfs_target
                    .ok_or_else(|| format!("volume mount entry {idx} missing nfs_target"))?;
                if staging_path.is_some() {
                    return Err(format!(
                        "volume mount entry {idx} with transport nfs must not set staging_path"
                    ));
                }
                parse_nfs_target(&target, &volume_id)
                    .map_err(|e| format!("volume mount entry {idx} {e}"))?;
                (String::new(), target)
            }
            VolumeTransport::Bind => {
                let staging_path = staging_path
                    .ok_or_else(|| format!("volume mount entry {idx} missing staging_path"))?;
                let expected_staging = format!("/run/cubepod/volumes/{volume_id}");
                if staging_path != expected_staging {
                    return Err(format!(
                        "volume mount entry {idx} has invalid staging_path {staging_path:?}: expected {expected_staging:?}"
                    ));
                }
                (staging_path, String::new())
            }
        };

        let norm_target = normalize_abs_path(&target_path, "target_path")?;

        if paths_overlap(&norm_target, &norm_workspace) {
            return Err(format!(
                "volume mount target_path {norm_target:?} overlaps workspace_dir {norm_workspace:?}"
            ));
        }
        for reserved in VOLUME_RESERVED_PATHS {
            if paths_overlap(&norm_target, reserved) {
                return Err(format!(
                    "volume mount target_path {norm_target:?} overlaps reserved path {reserved:?}"
                ));
            }
        }
        for prev in &specs {
            if prev.target_path == norm_target {
                return Err(format!(
                    "duplicate volume mount target_path {norm_target:?}"
                ));
            }
            if paths_overlap(&norm_target, &prev.target_path) {
                return Err(format!(
                    "volume mount target_path {norm_target:?} overlaps mount {:?}",
                    prev.target_path
                ));
            }
        }
        seen_targets.insert(norm_target.clone());

        specs.push(VolumeMountSpec {
            volume_id,
            staging_path,
            target_path: norm_target,
            transport,
            nfs_target,
        });
    }

    Ok(specs)
}

pub fn mount_volumes(
    mounts: &[VolumeMountSpec],
    root: &Path,
    default_uid: u32,
    default_gid: u32,
    no_pivot: bool,
) -> Result<(), String> {
    for mount in mounts {
        let dst = root.join(mount.target_path.trim_start_matches('/'));
        if mount.transport == VolumeTransport::Nfs {
            fs::create_dir_all(&dst)
                .map_err(|e| format!("mkdir volume target {}: {e}", dst.display()))?;
            chown_path(&dst, default_uid, default_gid)?;
            mount_nfs_volume(mount, &dst)?;
            continue;
        }

        let src_path = Path::new(&mount.staging_path);
        if !src_path.is_dir() {
            return Err(format!(
                "volume {} staging path {} does not exist or is not a directory",
                mount.volume_id, mount.staging_path
            ));
        }

        fs::create_dir_all(&dst)
            .map_err(|e| format!("mkdir volume target {}: {e}", dst.display()))?;
        chown_path(&dst, default_uid, default_gid)?;

        // With no pivot, Kubernetes can mount the same PVC/subPath at both
        // locations. Do not issue mount(2) in that case: a standard container
        // intentionally has no CAP_SYS_ADMIN. Pivoted roots still use the
        // bind/remount path below, including its nosuid/nodev hardening.
        use std::os::unix::fs::MetadataExt;
        let src_meta = fs::metadata(src_path).map_err(|e| format!("stat volume source: {e}"))?;
        let dst_meta = fs::metadata(&dst).map_err(|e| format!("stat volume target: {e}"))?;
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

fn nfs_port_from_env() -> u16 {
    match std::env::var("CUBEPOD_VOLUME_NFS_PORT") {
        Ok(raw) => raw
            .trim()
            .parse::<u16>()
            .ok()
            .filter(|p| *p > 0)
            .unwrap_or(NFS_DEFAULT_PORT),
        Err(_) => NFS_DEFAULT_PORT,
    }
}

fn retry_until<F, C>(
    mut attempt: F,
    deadline: Instant,
    confirm: C,
    what: &str,
) -> Result<(), String>
where
    F: FnMut() -> std::io::Result<()>,
    C: FnOnce() -> Result<(), String>,
{
    loop {
        match attempt() {
            Ok(()) => return confirm(),
            Err(err) => {
                if Instant::now() >= deadline {
                    return Err(format!("{what} failed after retries: {err}"));
                }
                thread::sleep(NFS_MOUNT_RETRY);
            }
        }
    }
}

fn mount_nfs_volume(mount: &VolumeMountSpec, dst: &Path) -> Result<(), String> {
    let (addr, _lease) = parse_nfs_target(&mount.nfs_target, &mount.volume_id)?;
    let port = nfs_port_from_env();
    let data = nfs_mount_data(&addr, port);
    let c_src = CString::new(mount.nfs_target.as_bytes())
        .map_err(|e| format!("invalid nfs_target {}: {e}", mount.nfs_target))?;
    let c_dst = CString::new(dst.to_string_lossy().as_bytes())
        .map_err(|e| format!("invalid target_path {}: {e}", dst.display()))?;
    let c_fstype = CString::new(NFS_FSTYPE).map_err(|e| format!("invalid nfs fstype: {e}"))?;
    let c_data =
        CString::new(data.as_bytes()).map_err(|e| format!("invalid nfs mount data: {e}"))?;
    let flags = (libc::MS_NOSUID | libc::MS_NODEV) as libc::c_ulong;

    let deadline = Instant::now() + NFS_MOUNT_DEADLINE;
    retry_until(
        || {
            let rc = unsafe {
                libc::mount(
                    c_src.as_ptr(),
                    c_dst.as_ptr(),
                    c_fstype.as_ptr(),
                    flags,
                    c_data.as_ptr().cast(),
                )
            };
            if rc == 0 {
                Ok(())
            } else {
                Err(std::io::Error::last_os_error())
            }
        },
        deadline,
        || confirm_nfs_mount(dst),
        &format!("nfs mount {} -> {}", mount.nfs_target, dst.display()),
    )
}

fn confirm_nfs_mount(target: &Path) -> Result<(), String> {
    let raw = fs::read_to_string("/proc/mounts").map_err(|e| format!("read /proc/mounts: {e}"))?;
    confirm_nfs_mount_from(target, &raw)
}

/// Validate that the topmost (last) /proc/mounts entry for target is nfs/nfs3
/// with nosuid,nodev. nfs4 is rejected.
pub fn confirm_nfs_mount_from(target: &Path, raw: &str) -> Result<(), String> {
    let want = target.to_string_lossy();
    let mut last: Option<(String, String)> = None;
    for line in raw.lines() {
        let mut fields = line.split_whitespace();
        let Some(_src) = fields.next() else { continue };
        let Some(mountpoint) = fields.next() else {
            continue;
        };
        let Some(fstype) = fields.next() else {
            continue;
        };
        let opts = fields.next().unwrap_or("");
        let mp = unescape_mount_path(mountpoint);
        if mp == want {
            last = Some((fstype.to_string(), opts.to_string()));
        }
    }
    let Some((fstype, opts)) = last else {
        return Err(format!(
            "volume target {} is not listed as an nfs mount in /proc/mounts",
            want
        ));
    };
    if fstype != "nfs" && fstype != "nfs3" {
        return Err(format!(
            "volume target {} is mounted as {fstype}, expected nfs or nfs3",
            want
        ));
    }
    if !opts.split(',').any(|o| o == "nosuid") || !opts.split(',').any(|o| o == "nodev") {
        return Err(format!(
            "volume target {} nfs mount missing nosuid,nodev (opts={opts})",
            want
        ));
    }
    Ok(())
}

fn unescape_mount_path(raw: &str) -> String {
    let mut out = String::with_capacity(raw.len());
    let bytes = raw.as_bytes();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] == b'\\' && i + 3 < bytes.len() {
            let oct = &raw[i + 1..i + 4];
            if let Ok(v) = u8::from_str_radix(oct, 8) {
                out.push(char::from(v));
                i += 4;
                continue;
            }
        }
        out.push(bytes[i] as char);
        i += 1;
    }
    out
}

fn expect_json_string(val: JsonValue, idx: usize, field: &str) -> Result<String, String> {
    match val {
        JsonValue::String(s) => Ok(s),
        _ => Err(format!(
            "volume mount entry {idx} field {field} must be a string"
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
        assert!(content.contains("\"volume_bind_duration_ms\":"));
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
            volume_bind_duration_ms: 5,
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
        assert!(content.contains("\"volume_bind_duration_ms\": 5"));
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
            volume_bind_duration_ms: 7,
        };

        let json = state.to_json();
        let expected = "{\n  \"pivoted\": true,\n  \"rootfs_device\": \"/dev/cubepod-rootfs\",\n  \"rootfs_action\": \"mounted\",\n  \"workspace_device\": \"/dev/cubepod-workspace\",\n  \"workspace_action\": \"mounted\",\n  \"duration_ms\": 42,\n  \"volume_bind_duration_ms\": 7\n}\n";
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
    fn volume_mounts_valid_parsing_and_normalization() {
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

        let specs = parse_and_validate_volume_mounts(&raw, "/workspace").expect("valid mounts");
        assert_eq!(
            specs,
            vec![
                VolumeMountSpec {
                    volume_id: vid1.to_string(),
                    staging_path: format!("/run/cubepod/volumes/{vid1}"),
                    target_path: "/mnt/dataset".to_string(),
                    transport: VolumeTransport::Bind,
                    nfs_target: String::new(),
                },
                VolumeMountSpec {
                    volume_id: vid2.to_string(),
                    staging_path: format!("/run/cubepod/volumes/{vid2}"),
                    target_path: "/mnt/models".to_string(),
                    transport: VolumeTransport::Bind,
                    nfs_target: String::new(),
                },
            ]
        );

        // Empty array is valid
        assert!(parse_and_validate_volume_mounts("[]", "/workspace")
            .unwrap()
            .is_empty());

        // Sibling prefix like /workspace2 does not overlap /workspace
        let sibling = format!(
            r#"[{{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/workspace2"}}]"#
        );
        assert_eq!(
            parse_and_validate_volume_mounts(&sibling, "/workspace")
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn volume_mounts_rejects_invalid_volume_id() {
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
                parse_and_validate_volume_mounts(raw, "/workspace").is_err(),
                "expected error for {raw}"
            );
        }
    }

    #[test]
    fn volume_mounts_rejects_bad_staging_path() {
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
                parse_and_validate_volume_mounts(&raw, "/workspace").is_err(),
                "expected error for {raw}"
            );
        }
    }

    #[test]
    fn volume_mounts_rejects_reserved_workspace_traversal_and_duplicates() {
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
                parse_and_validate_volume_mounts(&raw, "/workspace").is_err(),
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
        let err = parse_and_validate_volume_mounts(&dup_raw, "/workspace").unwrap_err();
        assert!(err.contains("duplicate"), "unexpected error: {err}");

        // Overlapping nested target_path between two volume mounts
        let nested_raw = format!(
            r#"[
                {{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/mnt/ds"}},
                {{"volume_id":"{vid2}","staging_path":"/run/cubepod/volumes/{vid2}","target_path":"/mnt/ds/sub"}}
            ]"#
        );
        let err = parse_and_validate_volume_mounts(&nested_raw, "/workspace").unwrap_err();
        assert!(err.contains("overlaps mount"), "unexpected error: {err}");
    }

    #[test]
    fn volume_mounts_env_and_file_fallback() {
        let vid1 = "0123456789abcdef0123456789abcdef";
        let vid2 = "fedcba9876543210fedcba9876543210";
        let file_path = unique_temp_path("test-volume-mounts", "json");
        let _guard = TempFileGuard(file_path.clone());

        // Missing file and no env -> empty
        assert!(load_volume_mounts_from(None, &file_path, "/workspace")
            .unwrap()
            .is_empty());

        // File fallback when env is None or empty
        let file_json = format!(
            r#"[{{"volume_id":"{vid1}","staging_path":"/run/cubepod/volumes/{vid1}","target_path":"/mnt/from-file"}}]"#
        );
        fs::write(&file_path, file_json).expect("write volume-mounts.json");

        let from_file = load_volume_mounts_from(Some(""), &file_path, "/workspace").unwrap();
        assert_eq!(from_file.len(), 1);
        assert_eq!(from_file[0].target_path, "/mnt/from-file");

        // Non-empty env overrides file
        let env_json = format!(
            r#"[{{"volume_id":"{vid2}","staging_path":"/run/cubepod/volumes/{vid2}","target_path":"/mnt/from-env"}}]"#
        );
        let from_env = load_volume_mounts_from(Some(&env_json), &file_path, "/workspace").unwrap();
        assert_eq!(from_env.len(), 1);
        assert_eq!(from_env[0].target_path, "/mnt/from-env");
    }

    #[test]
    fn mount_volumes_fails_fast_when_staging_missing() {
        let vid = "0123456789abcdef0123456789abcdef";
        let spec = VolumeMountSpec {
            volume_id: vid.to_string(),
            staging_path: format!("/run/cubepod/volumes/{vid}-nonexistent"),
            target_path: "/mnt/dataset".to_string(),
            transport: VolumeTransport::Bind,
            nfs_target: String::new(),
        };
        let err = mount_volumes(&[spec], Path::new("/"), 1000, 1000, false).unwrap_err();
        assert!(err.contains("does not exist"), "unexpected error: {err}");
    }
    #[test]
    fn already_mounted_target_needs_no_mount_capability() {
        let root = unique_temp_path("volume-direct", "dir");
        let target = root.join("data");
        fs::create_dir_all(&target).unwrap();
        fs::write(target.join("persist.txt"), "real-volume").unwrap();
        let mount = VolumeMountSpec {
            volume_id: "0123456789abcdef0123456789abcdef".into(),
            staging_path: target.to_string_lossy().into_owned(),
            target_path: "/data".into(),
            transport: VolumeTransport::Bind,
            nfs_target: String::new(),
        };
        mount_volumes(
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

    #[test]
    fn nfs_mount_parses_transport_and_rejects_staging_or_missing_target() {
        let vid = "0123456789abcdef0123456789abcdef";
        let lease = "00112233445566778899aabbccddeeff";
        let ok = format!(
            r#"[{{"volume_id":"{vid}","target_path":"/data","transport":"nfs","nfs_target":"10.96.10.20:/{lease}/{vid}"}}]"#
        );
        let specs = parse_and_validate_volume_mounts(&ok, "/workspace").expect("valid nfs");
        assert_eq!(specs.len(), 1);
        assert_eq!(specs[0].transport, VolumeTransport::Nfs);
        assert!(specs[0].staging_path.is_empty());
        assert_eq!(specs[0].nfs_target, format!("10.96.10.20:/{lease}/{vid}"));

        let missing =
            format!(r#"[{{"volume_id":"{vid}","target_path":"/data","transport":"nfs"}}]"#);
        let err = parse_and_validate_volume_mounts(&missing, "/workspace").unwrap_err();
        assert!(err.contains("nfs_target"), "{err}");

        let with_staging = format!(
            r#"[{{"volume_id":"{vid}","target_path":"/data","transport":"nfs","nfs_target":"10.96.10.20:/{lease}/{vid}","staging_path":"/run/cubepod/volumes/{vid}"}}]"#
        );
        let err = parse_and_validate_volume_mounts(&with_staging, "/workspace").unwrap_err();
        assert!(err.contains("staging_path"), "{err}");

        let unknown = format!(
            r#"[{{"volume_id":"{vid}","target_path":"/data","transport":"virtio","staging_path":"/run/cubepod/volumes/{vid}"}}]"#
        );
        let err = parse_and_validate_volume_mounts(&unknown, "/workspace").unwrap_err();
        assert!(err.contains("unknown transport"), "{err}");
    }

    #[test]
    fn bind_entries_without_transport_still_work() {
        let vid = "0123456789abcdef0123456789abcdef";
        let raw = format!(
            r#"[{{"volume_id":"{vid}","staging_path":"/run/cubepod/volumes/{vid}","target_path":"/mnt/data"}}]"#
        );
        let specs = parse_and_validate_volume_mounts(&raw, "/workspace").unwrap();
        assert_eq!(specs[0].transport, VolumeTransport::Bind);
    }

    #[test]
    fn nfs_mount_data_is_kernel_text_options_without_userspace_policy() {
        let data = nfs_mount_data("10.96.10.20", 2049);
        assert!(data.starts_with("addr=10.96.10.20,"));
        assert!(data.contains("vers=3"));
        assert!(data.contains("proto=tcp"));
        assert!(data.contains("port=2049"));
        assert!(data.contains("mountproto=tcp"));
        assert!(data.contains("mountport=2049"));
        assert!(data.contains("hard"));
        assert!(data.contains("nolock"));
        assert!(!data.contains("fg"));
        assert!(!data.contains("retry="));
        assert!(!data.contains("bg,") && !data.ends_with("bg"));
    }

    #[test]
    fn nfs_reserved_path_still_fails() {
        let vid = "0123456789abcdef0123456789abcdef";
        let lease = "00112233445566778899aabbccddeeff";
        let raw = format!(
            r#"[{{"volume_id":"{vid}","target_path":"/workspace","transport":"nfs","nfs_target":"10.96.10.20:/{lease}/{vid}"}}]"#
        );
        let err = parse_and_validate_volume_mounts(&raw, "/workspace").unwrap_err();
        assert!(err.contains("overlaps workspace_dir"), "{err}");
    }

    #[test]
    fn parse_nfs_target_requires_ip_literal() {
        let vid = "0123456789abcdef0123456789abcdef";
        let lease = "00112233445566778899aabbccddeeff";
        parse_nfs_target(&format!("10.96.10.20:/{lease}/{vid}"), vid).unwrap();
        parse_nfs_target(&format!("[2001:db8::20]:/{lease}/{vid}"), vid).unwrap();
        let err = parse_nfs_target(&format!("nfs.internal:/{lease}/{vid}"), vid).unwrap_err();
        assert!(err.contains("IP literal"), "{err}");
        let err = parse_nfs_target(&format!("10.0.0.1,vers=4:/{lease}/{vid}"), vid).unwrap_err();
        assert!(
            err.contains("IP literal") || err.contains("invalid"),
            "{err}"
        );
    }

    #[test]
    fn confirm_nfs_mount_uses_topmost_and_rejects_nfs4() {
        let ok = "src /data ext4 rw 0 0\n1.2.3.4:/lease/vol /data nfs4 rw,nosuid,nodev 0 0\n1.2.3.4:/lease/vol /data nfs rw,nosuid,nodev 0 0\n";
        confirm_nfs_mount_from(Path::new("/data"), ok).unwrap();

        let nfs4_top = "1.2.3.4:/l/v /data nfs rw,nosuid,nodev 0 0\n1.2.3.4:/l/v /data nfs4 rw,nosuid,nodev 0 0\n";
        let err = confirm_nfs_mount_from(Path::new("/data"), nfs4_top).unwrap_err();
        assert!(err.contains("nfs4"), "{err}");

        let missing = "1.2.3.4:/l/v /data nfs rw,nosuid 0 0\n";
        let err = confirm_nfs_mount_from(Path::new("/data"), missing).unwrap_err();
        assert!(err.contains("nosuid,nodev"), "{err}");

        let absent = "src /other nfs rw,nosuid,nodev 0 0\n";
        let err = confirm_nfs_mount_from(Path::new("/data"), absent).unwrap_err();
        assert!(err.contains("not listed"), "{err}");
    }

    #[test]
    fn nfs_mount_retry_fails_closed_after_deadline() {
        let deadline = Instant::now() + Duration::from_millis(80);
        let mut attempts = 0;
        let err = retry_until(
            || {
                attempts += 1;
                Err(std::io::Error::from_raw_os_error(libc::EIO))
            },
            deadline,
            || Ok(()),
            "nfs mount test -> /data",
        )
        .unwrap_err();
        assert!(attempts >= 1, "attempts={attempts}");
        assert!(err.contains("failed after retries"), "{err}");
    }

    #[test]
    fn nfs_mount_retry_runs_confirm_after_success() {
        let err = retry_until(
            || Ok(()),
            Instant::now() + Duration::from_secs(1),
            || Err("volume target /data is mounted as ext4, expected nfs or nfs3".into()),
            "nfs mount test -> /data",
        )
        .unwrap_err();
        assert!(err.contains("expected nfs"), "{err}");
    }

    #[test]
    fn mkfs_ext4_args_uses_lazy_init_and_nodiscard() {
        let args = mkfs_ext4_args("cubepod-rootfs");
        assert_eq!(
            args,
            [
                "-F",
                "-L",
                "cubepod-rootfs",
                "-m",
                "0",
                "-E",
                "lazy_itable_init=1,lazy_journal_init=1,nodiscard",
            ]
        );
    }

    #[test]
    fn overlay_mount_options_formats_expected_string() {
        let opts = overlay_mount_options(
            Path::new("/"),
            Path::new("/mnt/cubepod-upper/upper"),
            Path::new("/mnt/cubepod-upper/work"),
        );
        assert_eq!(
            opts,
            "lowerdir=/,upperdir=/mnt/cubepod-upper/upper,workdir=/mnt/cubepod-upper/work"
        );
    }

    #[test]
    fn ensure_and_mount_ext4_formats_blank_image_and_skips_fsck() {
        let img_path = unique_temp_path("test-mount-fresh", "img");
        let _guard = TempFileGuard(img_path.clone());
        let file = fs::File::create(&img_path).expect("create sparse file");
        file.set_len(64 * 1024 * 1024).expect("truncate to 64MB");
        drop(file);

        let target = Path::new("/mnt/test-upper");
        let mut mount_calls = 0usize;
        let mut fsck_calls = 0usize;

        let action = ensure_and_mount_ext4_with(
            &img_path,
            target,
            "cubepod-test",
            |_, _| {
                mount_calls += 1;
                Ok(())
            },
            |_| {
                fsck_calls += 1;
                Ok(VolumeAction::Mounted)
            },
        )
        .expect("fresh image should format and mount");

        assert_eq!(action, VolumeAction::Formatted);
        assert_eq!(mount_calls, 1);
        assert_eq!(fsck_calls, 0);
    }

    #[test]
    fn ensure_and_mount_ext4_direct_mount_skips_e2fsck_on_wake() {
        let img_path = unique_temp_path("test-mount-wake", "img");
        let _guard = TempFileGuard(img_path.clone());
        let file = fs::File::create(&img_path).expect("create sparse file");
        file.set_len(64 * 1024 * 1024).expect("truncate to 64MB");
        drop(file);

        format_ext4(&img_path, "cubepod-test").expect("format ext4");

        let target = Path::new("/mnt/test-upper");
        let mut mount_calls = 0usize;
        let mut fsck_calls = 0usize;

        let action = ensure_and_mount_ext4_with(
            &img_path,
            target,
            "cubepod-test",
            |_, _| {
                mount_calls += 1;
                Ok(())
            },
            |_| {
                fsck_calls += 1;
                Ok(VolumeAction::Healed)
            },
        )
        .expect("existing ext4 should direct-mount without fsck");

        assert_eq!(action, VolumeAction::Mounted);
        assert_eq!(mount_calls, 1);
        assert_eq!(fsck_calls, 0);
    }

    #[test]
    fn ensure_and_mount_ext4_falls_back_to_e2fsck_when_direct_mount_fails() {
        let img_path = unique_temp_path("test-mount-fallback", "img");
        let _guard = TempFileGuard(img_path.clone());
        let file = fs::File::create(&img_path).expect("create sparse file");
        file.set_len(64 * 1024 * 1024).expect("truncate to 64MB");
        drop(file);

        format_ext4(&img_path, "cubepod-test").expect("format ext4");

        let target = Path::new("/mnt/test-upper");
        let mut mount_calls = 0usize;
        let mut fsck_calls = 0usize;

        let action = ensure_and_mount_ext4_with(
            &img_path,
            target,
            "cubepod-test",
            |_, _| {
                mount_calls += 1;
                if mount_calls == 1 {
                    Err("Structure needs cleaning (EUCLEAN)".to_string())
                } else {
                    Ok(())
                }
            },
            |_| {
                fsck_calls += 1;
                Ok(VolumeAction::Healed)
            },
        )
        .expect("should heal with fsck and retry mount");

        assert_eq!(action, VolumeAction::Healed);
        assert_eq!(mount_calls, 2);
        assert_eq!(fsck_calls, 1);
    }

    #[test]
    fn ensure_and_mount_ext4_propagates_fsck_failure() {
        let img_path = unique_temp_path("test-mount-fsck-err", "img");
        let _guard = TempFileGuard(img_path.clone());
        let file = fs::File::create(&img_path).expect("create sparse file");
        file.set_len(64 * 1024 * 1024).expect("truncate to 64MB");
        drop(file);

        format_ext4(&img_path, "cubepod-test").expect("format ext4");

        let err = ensure_and_mount_ext4_with(
            &img_path,
            Path::new("/mnt/test-upper"),
            "cubepod-test",
            |_, _| Err("mount failed".to_string()),
            |_| Err("e2fsck -p failed with exit code Some(4)".to_string()),
        )
        .unwrap_err();

        assert!(err.contains("e2fsck -p failed"), "{err}");
    }
}
