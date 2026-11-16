//! Guest Resource Sampling & Prometheus Metrics for `podd`.
//!
//! Exposes:
//! - `GET /metrics`: E2B-compatible `SandboxMetric` JSON snapshot (CPU, memory, cache, disk)
//!   or Prometheus text exposition when `?format=prometheus` or `Accept: text/plain` is sent.
//! - `GET /v1/prometheus`: Prometheus text exposition (`0.0.4`) reporting active processes
//!   and `podd-init` boot/E2B bind durations from `/run/cubepod/init-state.json`.

use std::collections::HashMap;
use std::env;
use std::ffi::CString;
use std::fs;
use std::net::TcpStream;
use std::path::Path;
use std::sync::{Mutex, OnceLock};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::commands::ProcessRegistry;
use crate::http::{write_json, write_response, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::DaemonConfig;

const DEFAULT_INIT_STATE_PATH: &str = "/run/cubepod/init-state.json";

static PREV_CPU_SAMPLE: OnceLock<Mutex<Option<(u64, u64)>>> = OnceLock::new();

#[derive(Clone, Debug, PartialEq)]
pub struct SandboxMetric {
    pub timestamp: String,
    pub timestamp_unix: i64,
    pub cpu_count: usize,
    pub cpu_used_pct: f64,
    pub mem_used: u64,
    pub mem_total: u64,
    pub mem_cache: u64,
    pub disk_used: u64,
    pub disk_total: u64,
}

impl SandboxMetric {
    pub fn to_json(&self) -> String {
        format!(
            r#"{{"timestamp":"{}","timestampUnix":{},"cpuCount":{},"cpuUsedPct":{:.2},"memUsed":{},"memTotal":{},"memCache":{},"diskUsed":{},"diskTotal":{}}}"#,
            escape_json(&self.timestamp),
            self.timestamp_unix,
            self.cpu_count,
            self.cpu_used_pct,
            self.mem_used,
            self.mem_total,
            self.mem_cache,
            self.disk_used,
            self.disk_total
        )
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct InitMetrics {
    pub duration_ms: u64,
    pub e2b_bind_duration_ms: u64,
}

pub fn read_init_metrics(path: &Path) -> InitMetrics {
    let Ok(raw) = fs::read_to_string(path) else {
        return InitMetrics::default();
    };
    let Ok(val) = JsonValue::parse(&raw) else {
        return InitMetrics::default();
    };
    InitMetrics {
        duration_ms: val.get("duration_ms").and_then(|v| v.as_u64()).unwrap_or(0),
        e2b_bind_duration_ms: val
            .get("e2b_bind_duration_ms")
            .and_then(|v| v.as_u64())
            .unwrap_or(0),
    }
}

pub fn init_state_path() -> String {
    env::var("CUBEPOD_INIT_STATE_PATH").unwrap_or_else(|_| DEFAULT_INIT_STATE_PATH.to_string())
}

pub fn format_rfc3339_utc(unix_secs: i64) -> String {
    let days = unix_secs.div_euclid(86_400);
    let secs_of_day = unix_secs.rem_euclid(86_400);
    let hour = secs_of_day / 3600;
    let min = (secs_of_day % 3600) / 60;
    let sec = secs_of_day % 60;

    // Howard Hinnant's civil-from-days algorithm.
    let z = days + 719_468;
    let era = if z >= 0 { z } else { z - 146_096 } / 146_097;
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let y = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = y + if month <= 2 { 1 } else { 0 };
    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{min:02}:{sec:02}Z")
}

pub fn parse_proc_stat_cpu(contents: &str) -> Option<(u64, u64)> {
    for line in contents.lines() {
        let mut parts = line.split_whitespace();
        if parts.next() != Some("cpu") {
            continue;
        }
        let nums: Vec<u64> = parts.filter_map(|p| p.parse::<u64>().ok()).collect();
        if nums.len() < 4 {
            return None;
        }
        let total: u64 = nums.iter().copied().fold(0u64, u64::saturating_add);
        let idle = nums[3].saturating_add(nums.get(4).copied().unwrap_or(0));
        return Some((total, idle));
    }
    None
}

pub fn compute_cpu_used_pct(prev: (u64, u64), curr: (u64, u64)) -> f64 {
    let total_delta = curr.0.saturating_sub(prev.0);
    if total_delta == 0 {
        return 0.0;
    }
    let idle_delta = curr.1.saturating_sub(prev.1).min(total_delta);
    let busy = total_delta - idle_delta;
    let pct = (busy as f64 / total_delta as f64) * 100.0;
    if !pct.is_finite() {
        return 0.0;
    }
    ((pct * 100.0).round() / 100.0).clamp(0.0, 100.0)
}

pub fn parse_cgroup_cpu_count(cpu_max: Option<&str>) -> Option<usize> {
    let raw = cpu_max?.trim();
    let mut parts = raw.split_whitespace();
    let quota_str = parts.next()?;
    if quota_str == "max" {
        return None;
    }
    let quota: u64 = quota_str.parse().ok()?;
    let period: u64 = parts.next().and_then(|s| s.parse().ok()).unwrap_or(100_000);
    if period == 0 || quota == 0 {
        return None;
    }
    let cpus = quota.div_ceil(period) as usize;
    Some(cpus.max(1))
}

pub fn parse_cgroup_v1_cpu_count(
    quota_raw: Option<&str>,
    period_raw: Option<&str>,
) -> Option<usize> {
    let quota: i64 = quota_raw?.trim().parse().ok()?;
    if quota <= 0 {
        return None;
    }
    let period: i64 = period_raw
        .and_then(|s| s.trim().parse().ok())
        .unwrap_or(100_000);
    if period <= 0 {
        return None;
    }
    let cpus = (quota as u64).div_ceil(period as u64) as usize;
    Some(cpus.max(1))
}

pub fn sample_cpu_count() -> usize {
    if let Ok(cpu_max) = fs::read_to_string("/sys/fs/cgroup/cpu.max") {
        if let Some(n) = parse_cgroup_cpu_count(Some(&cpu_max)) {
            return n;
        }
    }
    if let Ok(quota_v1) = fs::read_to_string("/sys/fs/cgroup/cpu/cpu.cfs_quota_us") {
        let period_v1 = fs::read_to_string("/sys/fs/cgroup/cpu/cpu.cfs_period_us").ok();
        if let Some(n) = parse_cgroup_v1_cpu_count(Some(&quota_v1), period_v1.as_deref()) {
            return n;
        }
    }
    std::thread::available_parallelism()
        .map(|n| n.get())
        .unwrap_or(1)
        .max(1)
}

fn sample_loadavg_pct(cpu_count: usize) -> f64 {
    let Ok(raw) = fs::read_to_string("/proc/loadavg") else {
        return 0.0;
    };
    let Some(first) = raw.split_whitespace().next() else {
        return 0.0;
    };
    let Ok(load1) = first.parse::<f64>() else {
        return 0.0;
    };
    if !load1.is_finite() || load1 < 0.0 {
        return 0.0;
    }
    let cpus = cpu_count.max(1) as f64;
    let pct = (load1 / cpus) * 100.0;
    ((pct * 100.0).round() / 100.0).clamp(0.0, 100.0)
}

pub fn sample_cpu_used_pct(cpu_count: usize) -> f64 {
    let lock = PREV_CPU_SAMPLE.get_or_init(|| Mutex::new(None));
    let mut guard = lock.lock().unwrap_or_else(|e| e.into_inner());

    let first = fs::read_to_string("/proc/stat")
        .ok()
        .and_then(|s| parse_proc_stat_cpu(&s));

    let Some(sample1) = first else {
        return sample_loadavg_pct(cpu_count);
    };

    if let Some(prev) = *guard {
        if sample1.0 > prev.0 {
            *guard = Some(sample1);
            return compute_cpu_used_pct(prev, sample1);
        }
    }

    // No prior baseline (or same jiffy tick): take a brief 20ms delta read.
    drop(guard);
    std::thread::sleep(Duration::from_millis(20));
    let second = fs::read_to_string("/proc/stat")
        .ok()
        .and_then(|s| parse_proc_stat_cpu(&s));

    let mut guard = lock.lock().unwrap_or_else(|e| e.into_inner());
    match second {
        Some(sample2) if sample2.0 > sample1.0 => {
            *guard = Some(sample2);
            compute_cpu_used_pct(sample1, sample2)
        }
        Some(sample2) => {
            *guard = Some(sample2);
            sample_loadavg_pct(cpu_count)
        }
        None => {
            *guard = Some(sample1);
            sample_loadavg_pct(cpu_count)
        }
    }
}

pub fn parse_meminfo(contents: &str) -> Option<(u64, u64, u64)> {
    let mut total_kb: Option<u64> = None;
    let mut available_kb: Option<u64> = None;
    let mut free_kb: u64 = 0;
    let mut buffers_kb: u64 = 0;
    let mut cached_kb: u64 = 0;
    let mut sreclaimable_kb: u64 = 0;

    for line in contents.lines() {
        let Some((key, rest)) = line.split_once(':') else {
            continue;
        };
        let Some(val_kb) = rest
            .split_whitespace()
            .next()
            .and_then(|s| s.parse::<u64>().ok())
        else {
            continue;
        };
        match key.trim() {
            "MemTotal" => total_kb = Some(val_kb),
            "MemAvailable" => available_kb = Some(val_kb),
            "MemFree" => free_kb = val_kb,
            "Buffers" => buffers_kb = val_kb,
            "Cached" => cached_kb = val_kb,
            "SReclaimable" => sreclaimable_kb = val_kb,
            _ => {}
        }
    }

    let total = total_kb?.saturating_mul(1024);
    let cache = cached_kb
        .saturating_add(buffers_kb)
        .saturating_add(sreclaimable_kb)
        .saturating_mul(1024)
        .min(total);
    let used = match available_kb {
        Some(avail) => total.saturating_sub(avail.saturating_mul(1024)),
        None => total.saturating_sub(free_kb.saturating_mul(1024).saturating_add(cache)),
    };
    Some((used.min(total), total, cache))
}

pub fn apply_cgroup_memory(
    mem_used: u64,
    mem_total: u64,
    mem_cache: u64,
    cg_current: Option<&str>,
    cg_max: Option<&str>,
    cg_stat: Option<&str>,
) -> (u64, u64, u64) {
    let mut total = mem_total;
    let mut used = mem_used;
    let mut cache = mem_cache;

    let max_limit = cg_max
        .map(str::trim)
        .filter(|s| !s.is_empty() && *s != "max")
        .and_then(|s| s.parse::<u64>().ok())
        .filter(|&v| v > 0 && (total == 0 || v < total));

    if let Some(limit) = max_limit {
        total = limit;
        if let Some(cur) = cg_current
            .map(str::trim)
            .and_then(|s| s.parse::<u64>().ok())
        {
            used = cur.min(total);
        } else {
            used = used.min(total);
        }
        if let Some(stat_raw) = cg_stat {
            let mut file_bytes: Option<u64> = None;
            let mut cache_bytes: Option<u64> = None;
            for line in stat_raw.lines() {
                let mut parts = line.split_whitespace();
                match (
                    parts.next(),
                    parts.next().and_then(|v| v.parse::<u64>().ok()),
                ) {
                    (Some("file"), Some(v)) => file_bytes = Some(v),
                    (Some("cache"), Some(v)) => cache_bytes = Some(v),
                    _ => {}
                }
            }
            if let Some(c) = file_bytes.or(cache_bytes) {
                cache = c.min(total);
            }
        }
    }

    (used.min(total), total, cache.min(total))
}

pub fn sample_memory() -> (u64, u64, u64) {
    let (mut used, mut total, mut cache) = fs::read_to_string("/proc/meminfo")
        .ok()
        .and_then(|s| parse_meminfo(&s))
        .unwrap_or((0, 0, 0));

    // Try cgroup v2 first, then cgroup v1.
    let cg2_max = fs::read_to_string("/sys/fs/cgroup/memory.max").ok();
    if cg2_max.is_some() {
        let cg2_cur = fs::read_to_string("/sys/fs/cgroup/memory.current").ok();
        let cg2_stat = fs::read_to_string("/sys/fs/cgroup/memory.stat").ok();
        (used, total, cache) = apply_cgroup_memory(
            used,
            total,
            cache,
            cg2_cur.as_deref(),
            cg2_max.as_deref(),
            cg2_stat.as_deref(),
        );
    } else {
        let cg1_max = fs::read_to_string("/sys/fs/cgroup/memory/memory.limit_in_bytes").ok();
        let cg1_cur = fs::read_to_string("/sys/fs/cgroup/memory/memory.usage_in_bytes").ok();
        let cg1_stat = fs::read_to_string("/sys/fs/cgroup/memory/memory.stat").ok();
        (used, total, cache) = apply_cgroup_memory(
            used,
            total,
            cache,
            cg1_cur.as_deref(),
            cg1_max.as_deref(),
            cg1_stat.as_deref(),
        );
    }

    (used, total, cache)
}

#[allow(clippy::unnecessary_cast)]
pub fn statvfs_path(path: &str) -> Option<(u64, u64)> {
    let c_path = CString::new(path).ok()?;
    let mut st: libc::statvfs = unsafe { std::mem::zeroed() };
    let rc = unsafe { libc::statvfs(c_path.as_ptr(), &mut st) };
    if rc != 0 {
        return None;
    }
    let block_size = if st.f_frsize > 0 {
        st.f_frsize as u64
    } else {
        st.f_bsize as u64
    };
    let total = (st.f_blocks as u64).saturating_mul(block_size);
    let free = (st.f_bfree as u64).saturating_mul(block_size);
    let used = total.saturating_sub(free);
    Some((used, total))
}

pub fn sample_disk(workdir: &str) -> (u64, u64) {
    if !workdir.is_empty() && Path::new(workdir).exists() {
        if let Some(res) = statvfs_path(workdir) {
            return res;
        }
    }
    statvfs_path("/").unwrap_or((0, 0))
}

pub fn sample_sandbox_metric(workdir: &str) -> SandboxMetric {
    let now_secs = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64;
    let cpu_count = sample_cpu_count();
    let cpu_used_pct = sample_cpu_used_pct(cpu_count);
    let (mem_used, mem_total, mem_cache) = sample_memory();
    let (disk_used, disk_total) = sample_disk(workdir);

    SandboxMetric {
        timestamp: format_rfc3339_utc(now_secs),
        timestamp_unix: now_secs,
        cpu_count,
        cpu_used_pct,
        mem_used,
        mem_total,
        mem_cache,
        disk_used,
        disk_total,
    }
}

pub fn render_prometheus_metrics(
    active_processes: usize,
    init: InitMetrics,
    sample: &SandboxMetric,
) -> String {
    format!(
        concat!(
            "# HELP cubepod_podd_active_processes Current number of active processes managed by podd\n",
            "# TYPE cubepod_podd_active_processes gauge\n",
            "cubepod_podd_active_processes {}\n",
            "# HELP cubepod_podd_init_duration_milliseconds Duration of podd-init setup in milliseconds\n",
            "# TYPE cubepod_podd_init_duration_milliseconds gauge\n",
            "cubepod_podd_init_duration_milliseconds {}\n",
            "# HELP cubepod_podd_init_e2b_bind_duration_ms Duration of podd-init E2B volume bind mounts in milliseconds\n",
            "# TYPE cubepod_podd_init_e2b_bind_duration_ms gauge\n",
            "cubepod_podd_init_e2b_bind_duration_ms {}\n",
            "# HELP cubepod_podd_cpu_used_percent Current guest CPU utilization percentage\n",
            "# TYPE cubepod_podd_cpu_used_percent gauge\n",
            "cubepod_podd_cpu_used_percent {:.2}\n",
            "# HELP cubepod_podd_memory_used_bytes Current guest memory used in bytes\n",
            "# TYPE cubepod_podd_memory_used_bytes gauge\n",
            "cubepod_podd_memory_used_bytes {}\n",
            "# HELP cubepod_podd_memory_total_bytes Total guest memory in bytes\n",
            "# TYPE cubepod_podd_memory_total_bytes gauge\n",
            "cubepod_podd_memory_total_bytes {}\n",
            "# HELP cubepod_podd_disk_used_bytes Current guest disk used in bytes\n",
            "# TYPE cubepod_podd_disk_used_bytes gauge\n",
            "cubepod_podd_disk_used_bytes {}\n",
            "# HELP cubepod_podd_disk_total_bytes Total guest disk size in bytes\n",
            "# TYPE cubepod_podd_disk_total_bytes gauge\n",
            "cubepod_podd_disk_total_bytes {}\n"
        ),
        active_processes,
        init.duration_ms,
        init.e2b_bind_duration_ms,
        sample.cpu_used_pct,
        sample.mem_used,
        sample.mem_total,
        sample.disk_used,
        sample.disk_total
    )
}

pub fn handle_metrics(
    stream: &mut TcpStream,
    req: &HttpRequest,
    query_params: &HashMap<String, String>,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let format_prom = query_params
        .get("format")
        .map(|f| f.eq_ignore_ascii_case("prometheus"))
        .unwrap_or(false);
    let accept_plain = req
        .headers
        .get("accept")
        .map(|a| {
            let lower = a.to_ascii_lowercase();
            lower.contains("text/plain") && !lower.contains("application/json")
        })
        .unwrap_or(false);

    if format_prom || accept_plain {
        handle_prometheus(stream, config, registry);
        return;
    }

    let metric = sample_sandbox_metric(&config.workdir);
    write_json(stream, "200 OK", &metric.to_json());
}

pub fn handle_prometheus(
    stream: &mut TcpStream,
    config: &DaemonConfig,
    registry: &ProcessRegistry,
) {
    let active = registry.active_count();
    let init_path = init_state_path();
    let init = read_init_metrics(Path::new(&init_path));
    let sample = sample_sandbox_metric(&config.workdir);
    let body = render_prometheus_metrics(active, init, &sample);
    write_response(
        stream,
        "200 OK",
        "text/plain; version=0.0.4; charset=utf-8",
        body.as_bytes(),
    );
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

    static TEST_SEQ: AtomicUsize = AtomicUsize::new(1);

    fn temp_file(name: &str) -> std::path::PathBuf {
        let id = TEST_SEQ.fetch_add(1, Ordering::SeqCst);
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos();
        env::temp_dir().join(format!("podd-metrics-{name}-{nanos}-{id}.json"))
    }

    #[test]
    fn rfc3339_formatting_epoch_and_modern_dates() {
        assert_eq!(format_rfc3339_utc(0), "1970-01-01T00:00:00Z");
        assert_eq!(format_rfc3339_utc(1_700_000_000), "2023-11-14T22:13:20Z");
        assert_eq!(format_rfc3339_utc(1_794_830_400), "2026-11-16T12:00:00Z");
    }

    #[test]
    fn proc_stat_cpu_delta_calculation() {
        let stat1 = "cpu  100 10 40 800 50 0 0 0 0 0\ncpu0 50 5 20 400 25 0 0 0 0 0\n";
        let stat2 = "cpu  150 10 60 830 50 0 0 0 0 0\n";

        let s1 = parse_proc_stat_cpu(stat1).expect("parse stat1");
        let s2 = parse_proc_stat_cpu(stat2).expect("parse stat2");
        assert_eq!(s1, (1000, 850));
        assert_eq!(s2, (1100, 880));

        // Total delta = 100, idle delta = 30 => 70% busy.
        let pct = compute_cpu_used_pct(s1, s2);
        assert!((pct - 70.0).abs() < 0.01, "expected 70.0, got {pct}");
        assert_eq!(compute_cpu_used_pct(s2, s2), 0.0);
    }

    #[test]
    fn meminfo_and_cgroup_override_parsing() {
        let meminfo = "\
MemTotal:       16384000 kB
MemFree:         4000000 kB
MemAvailable:   12000000 kB
Buffers:          500000 kB
Cached:          2000000 kB
SReclaimable:     500000 kB
";
        let (used, total, cache) = parse_meminfo(meminfo).expect("parse meminfo");
        assert_eq!(total, 16_384_000 * 1024);
        assert_eq!(used, (16_384_000 - 12_000_000) * 1024);
        assert_eq!(cache, 3_000_000 * 1024);

        let (cg_used, cg_total, cg_cache) = apply_cgroup_memory(
            used,
            total,
            cache,
            Some("536870912\n"),
            Some("2147483648\n"),
            Some("anon 400000000\nfile 134217728\n"),
        );
        assert_eq!(cg_total, 2_147_483_648);
        assert_eq!(cg_used, 536_870_912);
        assert_eq!(cg_cache, 134_217_728);

        // "max" leaves host meminfo untouched
        let (u2, t2, c2) =
            apply_cgroup_memory(used, total, cache, Some("100"), Some("max\n"), None);
        assert_eq!((u2, t2, c2), (used, total, cache));
    }

    #[test]
    fn cgroup_cpu_max_parsing() {
        assert_eq!(parse_cgroup_cpu_count(Some("200000 100000\n")), Some(2));
        assert_eq!(parse_cgroup_cpu_count(Some("150000 100000")), Some(2));
        assert_eq!(parse_cgroup_cpu_count(Some("max 100000")), None);
        assert_eq!(
            parse_cgroup_v1_cpu_count(Some("250000\n"), Some("100000\n")),
            Some(3)
        );
        assert_eq!(
            parse_cgroup_v1_cpu_count(Some("-1\n"), Some("100000")),
            None
        );
        assert!(parse_proc_stat_cpu("cpu 18446744073709551610 10 10 10\n").is_some());
    }

    #[test]
    fn read_init_state_metrics_parses_durations_and_handles_missing() {
        let missing = temp_file("missing");
        assert_eq!(read_init_metrics(&missing), InitMetrics::default());

        let present = temp_file("present");
        fs::write(
            &present,
            r#"{"pivoted":true,"duration_ms":85,"e2b_bind_duration_ms":12}"#,
        )
        .unwrap();
        let parsed = read_init_metrics(&present);
        let _ = fs::remove_file(&present);
        assert_eq!(
            parsed,
            InitMetrics {
                duration_ms: 85,
                e2b_bind_duration_ms: 12,
            }
        );
    }

    #[test]
    fn sample_disk_and_prometheus_rendering() {
        let (used, total) = sample_disk("/tmp");
        assert!(total > 0, "expected /tmp statvfs total > 0");
        assert!(used <= total, "expected used <= total");

        let sample = SandboxMetric {
            timestamp: "2026-11-16T12:00:00Z".to_string(),
            timestamp_unix: 1_794_830_400,
            cpu_count: 2,
            cpu_used_pct: 12.5,
            mem_used: 1024,
            mem_total: 4096,
            mem_cache: 512,
            disk_used: 2048,
            disk_total: 8192,
        };
        let prom = render_prometheus_metrics(
            3,
            InitMetrics {
                duration_ms: 42,
                e2b_bind_duration_ms: 7,
            },
            &sample,
        );
        assert!(prom.contains("cubepod_podd_active_processes 3\n"));
        assert!(prom.contains("cubepod_podd_init_duration_milliseconds 42\n"));
        assert!(prom.contains("cubepod_podd_init_e2b_bind_duration_ms 7\n"));
    }
}
