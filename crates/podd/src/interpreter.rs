//! Code Interpreter execution engine and Context management (conforming to CEP-0018).

use std::collections::HashMap;
use std::env;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpStream;
use std::process::{ChildStdin, ChildStdout, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::http::{write_json, write_response, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::startcmd::{
    configure_command_credentials, exit_code_from_status, spawn_tracked, TrackedChild,
};
use crate::DaemonConfig;

/// In-guest Python runner script passed to `python3 -u -c`.
/// Intercepts stdout/stderr, captures AST expression evaluation, rich display reprs (_repr_png_, _repr_html_),
/// captures matplotlib active figures, formats tracebacks, and emits NDJSON protocol messages.
pub const PYTHON_RUNNER_SCRIPT: &str = r#"
import sys, os, ast, json, time, traceback, builtins

real_stdout = sys.__stdout__

def send_msg(obj):
    line = json.dumps(obj) + "\n"
    real_stdout.write(line)
    real_stdout.flush()

class StreamEmitter:
    def __init__(self, stream_type):
        self.stream_type = stream_type
        self.buf = ""
    def write(self, s):
        if not s:
            return 0
        self.buf += s
        if "\n" in self.buf:
            parts = self.buf.split("\n")
            for p in parts[:-1]:
                send_msg({"type": self.stream_type, "text": p + "\n", "timestamp": int(time.time() * 1000)})
            self.buf = parts[-1]
            real_stdout.flush()
        return len(s)
    def flush(self):
        if self.buf:
            send_msg({"type": self.stream_type, "text": self.buf, "timestamp": int(time.time() * 1000)})
            self.buf = ""
        real_stdout.flush()

sys.stdout = StreamEmitter("stdout")
sys.stderr = StreamEmitter("stderr")

_orig_print = builtins.print
def _flush_print(*args, **kwargs):
    if "flush" not in kwargs:
        kwargs["flush"] = True
    _orig_print(*args, **kwargs)
    sys.stdout.flush()

builtins.print = _flush_print

def _no_input(prompt=None):
    raise EOFError("interactive input() is not supported in code interpreter execution")

user_globals = {
    "__name__": "__main__",
    "__doc__": None,
    "__builtins__": __builtins__,
    "input": _no_input,
    "print": _flush_print,
}

def emit_result(val):
    png_data = None
    html_data = None
    if hasattr(val, "_repr_png_"):
        try:
            res = val._repr_png_()
            if isinstance(res, bytes):
                import base64
                png_data = base64.b64encode(res).decode("ascii")
            elif isinstance(res, str):
                png_data = res
        except Exception:
            pass
    if hasattr(val, "_repr_html_"):
        try:
            html_data = str(val._repr_html_())
        except Exception:
            pass
    if "matplotlib.pyplot" in sys.modules and png_data is None:
        try:
            plt = sys.modules["matplotlib.pyplot"]
            if plt.get_fignums():
                import io, base64
                buf = io.BytesIO()
                plt.savefig(buf, format="png", bbox_inches="tight")
                buf.seek(0)
                png_data = base64.b64encode(buf.read()).decode("ascii")
                plt.close("all")
        except Exception:
            pass
    text_val = repr(val)
    send_msg({
        "type": "result",
        "text": text_val,
        "png": png_data,
        "html": html_data,
    })

def emit_error(exc):
    tb = "".join(traceback.format_exception(type(exc), exc, exc.__traceback__))
    send_msg({
        "type": "error",
        "name": type(exc).__name__,
        "value": str(exc),
        "traceback": tb,
    })

send_msg({"type": "_cubepod_ready"})

while True:
    req_line = sys.stdin.readline()
    if not req_line:
        break
    try:
        req = json.loads(req_line)
        action = req.get("action")
        if action == "execute":
            code = req.get("code", "")
            execution_count = req.get("execution_count", 1)
            env_vars = req.get("env_vars") or {}
            for k, v in env_vars.items():
                os.environ[k] = str(v)
            try:
                tree = ast.parse(code, filename="<stdin>")
                if tree.body and isinstance(tree.body[-1], ast.Expr):
                    if len(tree.body) > 1:
                        mod = ast.Module(body=tree.body[:-1], type_ignores=[])
                        exec(compile(mod, "<stdin>", "exec"), user_globals)
                    expr = ast.Expression(body=tree.body[-1].value)
                    res = eval(compile(expr, "<stdin>", "eval"), user_globals)
                    sys.stdout.flush()
                    sys.stderr.flush()
                    if res is not None:
                        emit_result(res)
                else:
                    exec(compile(tree, "<stdin>", "exec"), user_globals)
                    sys.stdout.flush()
                    sys.stderr.flush()
            except BaseException as exc:
                sys.stdout.flush()
                sys.stderr.flush()
                emit_error(exc)
            send_msg({
                "type": "number_of_executions",
                "execution_count": execution_count,
            })
            send_msg({"type": "_cubepod_done"})
    except Exception as e:
        send_msg({"type": "error", "name": type(e).__name__, "value": str(e), "traceback": ""})
        send_msg({"type": "_cubepod_done"})
"#;

pub struct WorkerProcess {
    pub child: TrackedChild,
    pub stdin: ChildStdin,
    pub reader: BufReader<ChildStdout>,
}

impl Drop for WorkerProcess {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

pub struct ExecutionContext {
    pub id: String,
    pub language: String,
    pub cwd: String,
    pub execution_count: u64,
    pub worker: Option<WorkerProcess>,
}

impl ExecutionContext {
    pub fn ensure_worker(&mut self) -> Result<&mut WorkerProcess, String> {
        if let Some(ref mut w) = self.worker {
            match w.child.try_wait() {
                Ok(None) => return Ok(self.worker.as_mut().unwrap()),
                _ => {
                    self.worker = None;
                }
            }
        }

        let py_bin = find_python();
        let _ = std::fs::create_dir_all(&self.cwd);
        let mut cmd = Command::new(&py_bin);
        cmd.arg("-u").arg("-c").arg(PYTHON_RUNNER_SCRIPT);
        if std::path::Path::new(&self.cwd).exists() {
            cmd.current_dir(&self.cwd);
        }
        cmd.stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit());
        configure_command_credentials(&mut cmd, None, None, None)
            .map_err(|e| format!("failed to configure python interpreter credentials: {e}"))?;

        let mut child = spawn_tracked(&mut cmd)
            .map_err(|e| format!("failed to spawn python interpreter '{py_bin}': {e}"))?;

        let stdin = child
            .stdin()
            .take()
            .ok_or_else(|| "failed to capture stdin".to_string())?;
        let stdout = child
            .stdout()
            .take()
            .ok_or_else(|| "failed to capture stdout".to_string())?;
        let mut reader = BufReader::new(stdout);

        let mut ready_line = String::new();
        reader
            .read_line(&mut ready_line)
            .map_err(|e| format!("failed to read startup handshake from interpreter: {e}"))?;

        if !ready_line.contains("_cubepod_ready") {
            let _ = child.kill();
            let _ = child.wait();
            return Err(format!("unexpected interpreter handshake: {ready_line}"));
        }

        self.worker = Some(WorkerProcess {
            child,
            stdin,
            reader,
        });

        Ok(self.worker.as_mut().unwrap())
    }
}

#[derive(Clone)]
pub struct ContextRegistry {
    pub entries: Arc<Mutex<HashMap<String, Arc<Mutex<ExecutionContext>>>>>,
    pub counter: Arc<AtomicU64>,
}

impl Default for ContextRegistry {
    fn default() -> Self {
        Self::new()
    }
}

impl ContextRegistry {
    pub fn new() -> Self {
        Self {
            entries: Arc::new(Mutex::new(HashMap::new())),
            counter: Arc::new(AtomicU64::new(1)),
        }
    }

    pub fn next_id(&self) -> String {
        let n = self.counter.fetch_add(1, Ordering::SeqCst);
        format!("ctx-{n}")
    }

    pub fn create(
        &self,
        id: Option<String>,
        language: Option<String>,
        cwd: Option<String>,
        config: &DaemonConfig,
    ) -> Arc<Mutex<ExecutionContext>> {
        let id = id.unwrap_or_else(|| self.next_id());
        let language = language
            .filter(|l| !l.is_empty())
            .unwrap_or_else(|| "python".to_string());
        let cwd = cwd
            .filter(|c| !c.is_empty())
            .unwrap_or_else(|| config.workdir.clone());

        let ctx = Arc::new(Mutex::new(ExecutionContext {
            id: id.clone(),
            language,
            cwd,
            execution_count: 0,
            worker: None,
        }));

        let mut map = self.entries.lock().unwrap();
        map.insert(id, Arc::clone(&ctx));
        ctx
    }

    pub fn get(&self, id: &str) -> Option<Arc<Mutex<ExecutionContext>>> {
        let map = self.entries.lock().unwrap();
        map.get(id).cloned()
    }

    pub fn remove(&self, id: &str) -> Option<Arc<Mutex<ExecutionContext>>> {
        let mut map = self.entries.lock().unwrap();
        map.remove(id)
    }

    pub fn list(&self) -> Vec<(String, String, String)> {
        let map = self.entries.lock().unwrap();
        let mut res = Vec::new();
        for ctx_arc in map.values() {
            let c = ctx_arc.lock().unwrap();
            res.push((c.id.clone(), c.language.clone(), c.cwd.clone()));
        }
        res.sort_by(|a, b| a.0.cmp(&b.0));
        res
    }

    pub fn get_or_create_default(&self, config: &DaemonConfig) -> Arc<Mutex<ExecutionContext>> {
        if let Some(existing) = self.get("default") {
            return existing;
        }
        self.create(
            Some("default".to_string()),
            Some("python".to_string()),
            Some(config.workdir.clone()),
            config,
        )
    }

    pub fn terminate_all(&self) {
        let map = self.entries.lock().unwrap();
        for ctx_arc in map.values() {
            let mut c = ctx_arc.lock().unwrap();
            if let Some(mut w) = c.worker.take() {
                let _ = w.child.kill();
                let _ = w.child.wait();
            }
        }
    }
}

pub fn find_python() -> String {
    if let Ok(py) = env::var("PYTHON") {
        if !py.is_empty() {
            return py;
        }
    }
    for candidate in &["python3", "python", "/usr/bin/python3", "/usr/bin/python"] {
        let mut cmd = Command::new(candidate);
        cmd.arg("--version")
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped());
        if let Ok(child) = spawn_tracked(&mut cmd) {
            if child
                .wait_with_output()
                .map(|o| o.status.success())
                .unwrap_or(false)
            {
                return candidate.to_string();
            }
        }
    }
    "python3".to_string()
}

pub fn handle_execute(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    contexts: &ContextRegistry,
) {
    let body_str = match std::str::from_utf8(&req.body) {
        Ok(s) => s,
        Err(_) => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"invalid_utf8_body"}"#,
            );
            return;
        }
    };

    let json = match JsonValue::parse(body_str) {
        Ok(j) => j,
        Err(e) => {
            let resp = format!(
                r#"{{"error":"invalid_json","details":"{}"}}"#,
                escape_json(&e)
            );
            write_json(stream, "400 Bad Request", &resp);
            return;
        }
    };

    let code = match json.get("code").and_then(|v| v.as_str()) {
        Some(c) => c,
        None => {
            write_json(
                stream,
                "400 Bad Request",
                r#"{"error":"missing_code_field"}"#,
            );
            return;
        }
    };

    let context_id = json
        .get("context_id")
        .or_else(|| json.get("contextId"))
        .and_then(|v| v.as_str());

    let ctx_arc = if let Some(cid) = context_id {
        match contexts.get(cid) {
            Some(c) => c,
            None => {
                write_json(stream, "404 Not Found", r#"{"error":"context_not_found"}"#);
                return;
            }
        }
    } else {
        contexts.get_or_create_default(config)
    };

    let language_opt = json.get("language").and_then(|v| v.as_str());
    let language = match language_opt {
        Some(l) if !l.is_empty() => l.to_string(),
        _ => {
            let c = ctx_arc.lock().unwrap();
            if c.language.is_empty() {
                "python".to_string()
            } else {
                c.language.clone()
            }
        }
    };

    let mut env_vars = HashMap::new();
    if let Some(envs) = json
        .get("env_vars")
        .or_else(|| json.get("envVars"))
        .or_else(|| json.get("envs"))
        .and_then(|v| v.as_object())
    {
        for (k, v) in envs {
            if let Some(val) = v.as_str() {
                env_vars.insert(k.clone(), val.to_string());
            }
        }
    }

    match language.as_str() {
        "bash" => execute_shell(stream, &ctx_arc, code, "/bin/bash", &env_vars),
        "sh" => execute_shell(stream, &ctx_arc, code, "/bin/sh", &env_vars),
        _ => execute_python(stream, &ctx_arc, code, &env_vars),
    }
}

fn execute_python(
    stream: &mut TcpStream,
    ctx_arc: &Arc<Mutex<ExecutionContext>>,
    code: &str,
    env_vars: &HashMap<String, String>,
) {
    let mut ctx = ctx_arc.lock().unwrap();
    ctx.execution_count += 1;
    let count = ctx.execution_count;

    let worker = match ctx.ensure_worker() {
        Ok(w) => w,
        Err(e) => {
            let error_json = format!(
                r#"{{"type":"error","name":"InterpreterError","value":"{}","traceback":""}}"#,
                escape_json(&e)
            );
            let count_json = format!(
                r#"{{"type":"number_of_executions","execution_count":{}}}"#,
                count
            );
            let body = format!("{error_json}\n{count_json}\n");
            write_response(stream, "200 OK", "application/x-ndjson", body.as_bytes());
            return;
        }
    };

    let mut env_obj_parts = Vec::new();
    for (k, v) in env_vars {
        env_obj_parts.push(format!(r#""{}":"{}""#, escape_json(k), escape_json(v)));
    }
    let env_json = format!("{{{}}}", env_obj_parts.join(","));

    let req_line = format!(
        r#"{{"action":"execute","code":"{}","execution_count":{},"env_vars":{}}}"#,
        escape_json(code),
        count,
        env_json
    );

    if worker.stdin.write_all(req_line.as_bytes()).is_err()
        || worker.stdin.write_all(b"\n").is_err()
        || worker.stdin.flush().is_err()
    {
        ctx.worker = None;
        let body = format!(
            "{}\n{{\"type\":\"number_of_executions\",\"execution_count\":{count}}}\n",
            r#"{"type":"error","name":"BrokenPipe","value":"failed to write to interpreter stdin","traceback":""}"#
        );
        write_response(stream, "200 OK", "application/x-ndjson", body.as_bytes());
        return;
    }

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        return;
    }

    let mut line = String::new();
    loop {
        line.clear();
        match worker.reader.read_line(&mut line) {
            Ok(0) => {
                ctx.worker = None;
                let err_line = format!(
                    "{}\n{{\"type\":\"number_of_executions\",\"execution_count\":{count}}}\n",
                    r#"{"type":"error","name":"ProcessTerminated","value":"kernel process terminated unexpectedly","traceback":""}"#
                );
                let _ = stream.write_all(err_line.as_bytes());
                let _ = stream.flush();
                break;
            }
            Ok(_) => {
                let trimmed = line.trim();
                if trimmed.is_empty() {
                    continue;
                }
                if trimmed.contains(r#""type": "_cubepod_done""#)
                    || trimmed.contains(r#""type":"_cubepod_done""#)
                {
                    break;
                }
                if trimmed.contains(r#""type": "_cubepod_ready""#)
                    || trimmed.contains(r#""type":"_cubepod_ready""#)
                {
                    continue;
                }
                if stream.write_all(line.as_bytes()).is_err() || stream.flush().is_err() {
                    break;
                }
            }
            Err(_) => {
                ctx.worker = None;
                break;
            }
        }
    }
}

fn execute_shell(
    stream: &mut TcpStream,
    ctx_arc: &Arc<Mutex<ExecutionContext>>,
    code: &str,
    shell_bin: &str,
    env_vars: &HashMap<String, String>,
) {
    let mut ctx = ctx_arc.lock().unwrap();
    ctx.execution_count += 1;
    let count = ctx.execution_count;
    let cwd = ctx.cwd.clone();
    drop(ctx);

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        return;
    }

    let _ = std::fs::create_dir_all(&cwd);
    let mut cmd = Command::new(shell_bin);
    cmd.arg("-c").arg(code);
    if std::path::Path::new(&cwd).exists() {
        cmd.current_dir(&cwd);
    }
    if let Err(e) = configure_command_credentials(&mut cmd, None, None, None) {
        let err_msg = format!(
            r#"{{"type":"error","name":"CredentialError","value":"{}","traceback":""}}"#,
            escape_json(&e)
        );
        let exec_cnt = format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
        let _ = stream.write_all(format!("{err_msg}\n{exec_cnt}\n").as_bytes());
        let _ = stream.flush();
        return;
    }
    for (k, v) in env_vars {
        cmd.env(k, v);
    }
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());

    let now_ms = || -> u64 {
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map(|d| d.as_millis() as u64)
            .unwrap_or(0)
    };

    match spawn_tracked(&mut cmd) {
        Ok(mut child) => {
            let stdout = child.stdout().take();
            let stderr = child.stderr().take();

            if let Some(out) = stdout {
                let reader = BufReader::new(out);
                for line in reader.lines().map_while(Result::ok) {
                    let msg = format!(
                        r#"{{"type":"stdout","text":"{}\n","timestamp":{}}}"#,
                        escape_json(&line),
                        now_ms()
                    );
                    let _ = stream.write_all(format!("{msg}\n").as_bytes());
                    let _ = stream.flush();
                }
            }

            if let Some(err) = stderr {
                let reader = BufReader::new(err);
                for line in reader.lines().map_while(Result::ok) {
                    let msg = format!(
                        r#"{{"type":"stderr","text":"{}\n","timestamp":{}}}"#,
                        escape_json(&line),
                        now_ms()
                    );
                    let _ = stream.write_all(format!("{msg}\n").as_bytes());
                    let _ = stream.flush();
                }
            }

            let status = child.wait();
            let code = status.map(|s| exit_code_from_status(&s)).unwrap_or(-1);
            if code != 0 {
                let err_msg = format!(
                    r#"{{"type":"error","name":"ProcessExitError","value":"process exited with code {}","traceback":""}}"#,
                    code
                );
                let _ = stream.write_all(format!("{err_msg}\n").as_bytes());
                let _ = stream.flush();
            }

            let exec_cnt =
                format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
            let _ = stream.write_all(format!("{exec_cnt}\n").as_bytes());
            let _ = stream.flush();
        }
        Err(e) => {
            let err_msg = format!(
                r#"{{"type":"error","name":"SpawnError","value":"{}","traceback":""}}"#,
                escape_json(&e.to_string())
            );
            let exec_cnt =
                format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
            let _ = stream.write_all(format!("{err_msg}\n{exec_cnt}\n").as_bytes());
            let _ = stream.flush();
        }
    }
}

pub fn handle_context_create(
    stream: &mut TcpStream,
    req: &HttpRequest,
    config: &DaemonConfig,
    contexts: &ContextRegistry,
) {
    let (id, language, cwd) = if !req.body.is_empty() {
        if let Ok(body_str) = std::str::from_utf8(&req.body) {
            if let Ok(json) = JsonValue::parse(body_str) {
                let id = json
                    .get("id")
                    .and_then(|v| v.as_str())
                    .map(|s| s.to_string());
                let language = json
                    .get("language")
                    .and_then(|v| v.as_str())
                    .map(|s| s.to_string());
                let cwd = json
                    .get("cwd")
                    .or_else(|| json.get("workdir"))
                    .and_then(|v| v.as_str())
                    .map(|s| s.to_string());
                (id, language, cwd)
            } else {
                (None, None, None)
            }
        } else {
            (None, None, None)
        }
    } else {
        (None, None, None)
    };

    let ctx = contexts.create(id, language, cwd, config);
    let c = ctx.lock().unwrap();
    let resp = format!(
        r#"{{"id":"{}","language":"{}","cwd":"{}"}}"#,
        escape_json(&c.id),
        escape_json(&c.language),
        escape_json(&c.cwd)
    );
    write_json(stream, "200 OK", &resp);
}

pub fn handle_context_list(stream: &mut TcpStream, contexts: &ContextRegistry) {
    let items = contexts.list();
    let mut parts = Vec::new();
    for (id, language, cwd) in items {
        parts.push(format!(
            r#"{{"id":"{}","language":"{}","cwd":"{}"}}"#,
            escape_json(&id),
            escape_json(&language),
            escape_json(&cwd)
        ));
    }
    let body = format!("[{}]", parts.join(","));
    write_json(stream, "200 OK", &body);
}

pub fn handle_context_get(stream: &mut TcpStream, id: &str, contexts: &ContextRegistry) {
    if let Some(ctx_arc) = contexts.get(id) {
        let c = ctx_arc.lock().unwrap();
        let resp = format!(
            r#"{{"id":"{}","language":"{}","cwd":"{}"}}"#,
            escape_json(&c.id),
            escape_json(&c.language),
            escape_json(&c.cwd)
        );
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"context_not_found"}"#);
    }
}

pub fn handle_context_delete(stream: &mut TcpStream, id: &str, contexts: &ContextRegistry) {
    if let Some(ctx_arc) = contexts.remove(id) {
        let mut c = ctx_arc.lock().unwrap();
        if let Some(mut w) = c.worker.take() {
            let _ = w.child.kill();
            let _ = w.child.wait();
        }
        let resp = format!(r#"{{"status":"ok","id":"{}"}}"#, escape_json(id));
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"context_not_found"}"#);
    }
}

pub fn handle_context_restart(stream: &mut TcpStream, id: &str, contexts: &ContextRegistry) {
    if let Some(ctx_arc) = contexts.get(id) {
        let mut c = ctx_arc.lock().unwrap();
        if let Some(mut w) = c.worker.take() {
            let _ = w.child.kill();
            let _ = w.child.wait();
        }
        c.execution_count = 0;
        let resp = format!(
            r#"{{"id":"{}","language":"{}","cwd":"{}"}}"#,
            escape_json(&c.id),
            escape_json(&c.language),
            escape_json(&c.cwd)
        );
        write_json(stream, "200 OK", &resp);
    } else {
        write_json(stream, "404 Not Found", r#"{"error":"context_not_found"}"#);
    }
}
