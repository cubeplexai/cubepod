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

use std::env;
use std::ffi::CString;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::os::unix::process::CommandExt;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode};

#[derive(Debug, Clone)]
pub struct InitConfig {
    pub rootfs_device: PathBuf,
    pub workspace_device: PathBuf,
    pub upper_mount_path: PathBuf,
    pub merged_root_path: PathBuf,
    pub workspace_dir: PathBuf,
    pub default_uid: u32,
    pub default_gid: u32,
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

        Self {
            rootfs_device: PathBuf::from(
                env::var("CUBEPOD_ROOTFS_DEV").unwrap_or_else(|_| "/dev/cubepod-rootfs".into()),
            ),
            workspace_device: PathBuf::from(
                env::var("CUBEPOD_WORKSPACE_DEV")
                    .unwrap_or_else(|_| "/dev/cubepod-workspace".into()),
            ),
            upper_mount_path: PathBuf::from("/mnt/cubepod-upper"),
            merged_root_path: PathBuf::from("/mnt/cubepod-merged"),
            workspace_dir: PathBuf::from("/workspace"),
            default_uid: 1000,
            default_gid: 1000,
            skip_pivot: env::var("CUBEPOD_SKIP_PIVOT").as_deref() == Ok("1"),
            next_argv,
        }
    }
}

/// Ensures `device` has a valid `ext4` filesystem; formats with `mkfs.ext4` if blank,
/// or runs `e2fsck -p` to auto-heal after unclean VM shutdowns.
pub fn ensure_ext4(device: &Path, label: &str) -> Result<(), String> {
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
        return Ok(());
    }

    eprintln!(
        "[podd-init] verifying existing ext4 volume {} with e2fsck -p",
        device.display()
    );
    let status = Command::new("e2fsck")
        .arg("-p")
        .arg(device)
        .status()
        .map_err(|e| format!("failed to spawn e2fsck: {e}"))?;

    // e2fsck exit code 0 = no errors, 1 = filesystem errors corrected, 2 = corrected (reboot suggested)
    match status.code() {
        Some(0..=2) => Ok(()),
        other => Err(format!(
            "e2fsck -p failed on {} with exit code {:?}",
            device.display(),
            other
        )),
    }
}

pub fn setup_and_pivot(cfg: &InitConfig) -> Result<(), String> {
    if cfg.skip_pivot {
        eprintln!("[podd-init] CUBEPOD_SKIP_PIVOT=1 set, skipping block mount and pivot_root");
        return Ok(());
    }

    if !cfg.rootfs_device.exists() {
        eprintln!(
            "[podd-init] no raw block device at {}, continuing on standard rootfs",
            cfg.rootfs_device.display()
        );
        return Ok(());
    }

    // 1. Heal or format RootFS Upper block device.
    ensure_ext4(&cfg.rootfs_device, "cubepod-rootfs")?;

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

    // 4. Mount Workspace block device at <merged>/workspace and chown to 1000:1000.
    let rel_ws = cfg
        .workspace_dir
        .strip_prefix("/")
        .unwrap_or(&cfg.workspace_dir);
    let merged_workspace = cfg.merged_root_path.join(rel_ws);
    fs::create_dir_all(&merged_workspace)
        .map_err(|e| format!("mkdir {}: {e}", merged_workspace.display()))?;

    if cfg.workspace_device.exists() {
        ensure_ext4(&cfg.workspace_device, "cubepod-ws")?;
        mount_ext4(&cfg.workspace_device, &merged_workspace)?;
    }
    chown_path(&merged_workspace, cfg.default_uid, cfg.default_gid)?;

    // 5. Bind-mount virtual filesystems, the injected /opt/cubepod runtime volume,
    // and K8s networking configs into the merged root so podd remains accessible after pivot_root
    // without polluting the user's persistent RootFS upperdir.
    for sys_dir in ["proc", "sys", "dev", "run", "opt/cubepod"] {
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

    eprintln!(
        "[podd-init] persistent OverlayFS root active (workdir={})",
        cfg.workspace_dir.display()
    );
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

    #[test]
    fn graceful_fallback_when_block_device_missing() {
        let mut cfg = InitConfig::from_env_and_args(vec![]);
        cfg.rootfs_device = PathBuf::from("/dev/nonexistent-cubepod-rootfs-test");
        assert!(setup_and_pivot(&cfg).is_ok());
    }

    #[test]
    fn default_next_argv_points_to_podd() {
        let cfg = InitConfig::from_env_and_args(vec![]);
        assert_eq!(cfg.next_argv, vec!["/opt/cubepod/bin/podd".to_string()]);
    }
}
