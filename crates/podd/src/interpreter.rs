//! Multi-language Code Interpreter execution engine and Context management (conforming to CEP-0018).
//!
//! Supported kernels (with normalized aliases):
//! - `python` (`py`, `python3`, or empty default): persistent Python AST kernel
//! - `bash` (`sh`): persistent `/bin/bash` kernel preserving `pwd`, shell & exported vars
//! - `javascript` (`js`, `node`, `nodejs`): persistent `node:vm` kernel (sync & top-level `await`)
//! - `typescript` (`ts`): persistent `node:vm` kernel with `stripTypeScriptTypes` / transpiler fallback
//! - `r`: persistent `R` / `Rscript` kernel
//! - `java`: persistent `jdk.jshell` kernel (with per-cell `java` fallback when `jshell` is absent)

use std::collections::HashMap;
use std::env;
use std::io::{BufRead, BufReader, Write};
use std::net::TcpStream;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::{ChildStdin, ChildStdout, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use crate::commands::base64_encode;
use crate::http::{write_json, write_response, HttpRequest};
use crate::json::{escape_json, JsonValue};
use crate::startcmd::{
    configure_command_credentials, exit_code_from_status, spawn_tracked, TrackedChild,
};
use crate::DaemonConfig;

/// Normalized code interpreter kernel language.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum KernelLanguage {
    Python,
    Bash,
    JavaScript,
    TypeScript,
    R,
    Java,
}

impl KernelLanguage {
    /// Normalizes a language string (empty defaults to `Python`).
    pub fn parse(raw: &str) -> Result<Self, String> {
        match raw.trim().to_ascii_lowercase().as_str() {
            "" | "python" | "py" | "python3" => Ok(Self::Python),
            "bash" | "sh" => Ok(Self::Bash),
            "javascript" | "js" | "node" | "nodejs" => Ok(Self::JavaScript),
            "typescript" | "ts" => Ok(Self::TypeScript),
            "r" => Ok(Self::R),
            "java" => Ok(Self::Java),
            other => Err(format!("unsupported language: {other}")),
        }
    }

    pub fn canonical_name(self) -> &'static str {
        match self {
            Self::Python => "python",
            Self::Bash => "bash",
            Self::JavaScript => "javascript",
            Self::TypeScript => "typescript",
            Self::R => "r",
            Self::Java => "java",
        }
    }
}

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

/// Persistent stateful Bash kernel worker reading `<count> <code_b64> <env_b64>` lines on stdin.
/// Preserves `pwd`, shell variables, exported variables, and functions across executions in the same context.
pub const BASH_RUNNER_SCRIPT: &str = r#"
exec 3>&1 4<&0 5>&2
__cp_tmp=$(mktemp -d)
trap 'rm -rf "$__cp_tmp"' EXIT
mkfifo "$__cp_tmp/out" "$__cp_tmp/err"
__cp_nonce="$$_cubepod"

if [[ "$(date +%s%3N 2>/dev/null)" =~ ^[0-9]+$ ]]; then
  __cp_ts() { date +%s%3N; }
else
  __cp_ts() { echo "$(date +%s)000"; }
fi

__cp_json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\r'/\\r}"
  s="${s//$'\t'/\\t}"
  printf '%s' "$s"
}

__cp_stream() {
  local stype="$1" fifo="$2" sentinel="$3" line
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" == *"$sentinel" ]]; then
      local prefix="${line%"$sentinel"}"
      if [ -n "$prefix" ]; then
        printf '{"type":"%s","text":"%s","timestamp":%s}\n' \
          "$stype" "$(__cp_json_escape "$prefix")" "$(__cp_ts)" >&3
      fi
      break
    fi
    printf '{"type":"%s","text":"%s\\n","timestamp":%s}\n' \
      "$stype" "$(__cp_json_escape "$line")" "$(__cp_ts)" >&3
  done < "$fifo"
}

printf '{"type":"_cubepod_ready"}\n' >&3

while IFS=' ' read -r __cp_cnt __cp_code_b64 __cp_env_b64 <&4; do
  [ -z "$__cp_cnt" ] && continue
  __cp_code=""
  if [ "$__cp_code_b64" != "-" ]; then
    __cp_code=$(printf '%s' "$__cp_code_b64" | base64 -d 2>/dev/null)
  fi
  __cp_env=""
  if [ -n "$__cp_env_b64" ] && [ "$__cp_env_b64" != "-" ]; then
    __cp_env=$(printf '%s' "$__cp_env_b64" | base64 -d 2>/dev/null)
  fi

  __cp_stream stdout "$__cp_tmp/out" "__CUBEPOD_SENTINEL_${__cp_nonce}__" &
  __cp_p1=$!
  __cp_stream stderr "$__cp_tmp/err" "__CUBEPOD_SENTINEL_${__cp_nonce}__" &
  __cp_p2=$!

  exec 1>"$__cp_tmp/out" 2>"$__cp_tmp/err"
  set +e
  if [ -n "$__cp_env" ]; then
    eval "$__cp_env" </dev/null
  fi
  eval "$__cp_code" </dev/null
  __cp_rc=$?
  set +e

  printf '%s\n' "__CUBEPOD_SENTINEL_${__cp_nonce}__" >&1
  printf '%s\n' "__CUBEPOD_SENTINEL_${__cp_nonce}__" >&2
  exec 1>&3 2>&5
  wait "$__cp_p1" "$__cp_p2" 2>/dev/null

  if [ "$__cp_rc" -ne 0 ]; then
    printf '{"type":"error","name":"ProcessExitError","value":"process exited with code %s","traceback":""}\n' "$__cp_rc" >&3
  fi
  printf '{"type":"number_of_executions","execution_count":%s}\n' "$__cp_cnt" >&3
  printf '{"type":"_cubepod_done"}\n' >&3
done
"#;

/// Persistent stateful Node.js (`node:vm`) runner for both `javascript` and `typescript`.
/// Supports synchronous expressions, top-level `await`, `console.*` / `process.stdout.write` capture,
/// and TypeScript stripping via `node:module.stripTypeScriptTypes` (or installed `typescript`/`tsx`/`ts-node`).
pub const NODE_RUNNER_SCRIPT: &str = r#"
const vm = require("node:vm");
const util = require("node:util");
const path = require("node:path");
const { Writable } = require("node:stream");
const { Console } = require("node:console");
const readline = require("node:readline");
const nodeModule = require("node:module");

const realStdoutWrite = process.stdout.write.bind(process.stdout);

function sendMsg(obj) {
  realStdoutWrite(JSON.stringify(obj) + "\n");
}

class StreamBuffer {
  constructor(streamType) {
    this.streamType = streamType;
    this.buf = "";
  }
  writeChunk(chunk, encoding) {
    const enc = typeof encoding === "string" && encoding !== "buffer" ? encoding : "utf8";
    const s = Buffer.isBuffer(chunk) ? chunk.toString(enc) : String(chunk);
    if (!s) return;
    this.buf += s;
    if (this.buf.includes("\n")) {
      const parts = this.buf.split("\n");
      for (let i = 0; i < parts.length - 1; i++) {
        sendMsg({
          type: this.streamType,
          text: parts[i] + "\n",
          timestamp: Date.now(),
        });
      }
      this.buf = parts[parts.length - 1];
    }
  }
  flush() {
    if (this.buf.length > 0) {
      sendMsg({
        type: this.streamType,
        text: this.buf,
        timestamp: Date.now(),
      });
      this.buf = "";
    }
  }
}

const outEmitter = new StreamBuffer("stdout");
const errEmitter = new StreamBuffer("stderr");

function makeWritable(emitter) {
  return new Writable({
    write(chunk, encoding, callback) {
      emitter.writeChunk(chunk, encoding);
      callback();
    },
  });
}

const outStream = makeWritable(outEmitter);
const errStream = makeWritable(errEmitter);
const kernelConsole = new Console({ stdout: outStream, stderr: errStream });

process.stdout.write = (chunk, encoding, cb) => {
  if (typeof encoding === "function") {
    cb = encoding;
    encoding = "utf8";
  }
  outEmitter.writeChunk(chunk, encoding);
  if (typeof cb === "function") cb();
  return true;
};

process.stderr.write = (chunk, encoding, cb) => {
  if (typeof encoding === "function") {
    cb = encoding;
    encoding = "utf8";
  }
  errEmitter.writeChunk(chunk, encoding);
  if (typeof cb === "function") cb();
  return true;
};

const sandbox = {
  console: kernelConsole,
  process,
  Buffer,
  setTimeout,
  setInterval,
  clearTimeout,
  clearInterval,
  setImmediate,
  clearImmediate,
  URL,
  URLSearchParams,
  TextEncoder,
  TextDecoder,
  fetch: globalThis.fetch,
  require,
  __dirname: process.cwd(),
  __filename: path.join(process.cwd(), "<stdin>"),
};
sandbox.global = sandbox;
sandbox.globalThis = sandbox;
const vmCtx = vm.createContext(sandbox);

function stripTypeScript(code) {
  if (typeof nodeModule.stripTypeScriptTypes === "function") {
    try {
      return nodeModule.stripTypeScriptTypes(code, { mode: "transform" });
    } catch (err) {
      if (err && (err.code === "ERR_INVALID_ARG_VALUE" || err.code === "ERR_INVALID_ARG_TYPE")) {
        return nodeModule.stripTypeScriptTypes(code);
      }
      throw err;
    }
  }
  for (const pkg of ["typescript", "tsx", "ts-node"]) {
    try {
      if (pkg === "typescript") {
        const ts = require("typescript");
        const out = ts.transpileModule(code, {
          compilerOptions: {
            target: ts.ScriptTarget.ES2022,
            module: ts.ModuleKind.CommonJS,
          },
        });
        return out.outputText;
      }
      if (pkg === "ts-node") {
        const tsNode = require("ts-node");
        const svc = tsNode.create({ transpileOnly: true });
        return svc.compile(code, "<stdin>.ts");
      }
    } catch (_) {
      // try next fallback
    }
  }
  const err = new Error(
    "TypeScript execution requires Node.js >= 22.6 (module.stripTypeScriptTypes) or 'typescript'/'ts-node' installed"
  );
  err.name = "TypeScriptUnsupportedError";
  throw err;
}

function hoistTopLevelDecls(code) {
  return code
    .split("\n")
    .map((line) => line.replace(/^(\s*)(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=/, "$1globalThis.$2 ="))
    .join("\n");
}

function buildAsyncWrapper(code) {
  const transformed = hoistTopLevelDecls(code);
  const lines = transformed.split("\n");
  let lastIdx = lines.length - 1;
  while (lastIdx >= 0 && lines[lastIdx].trim() === "") {
    lastIdx--;
  }
  if (lastIdx >= 0) {
    const lastLine = lines[lastIdx].trim().replace(/;+\s*$/, "");
    if (lastLine && !lastLine.startsWith("//")) {
      const prefix = lines.slice(0, lastIdx).join("\n");
      const candidate = `(async () => {\n${prefix}\nreturn (${lastLine});\n})()`;
      try {
        new vm.Script(candidate, { filename: "<stdin>" });
        return candidate;
      } catch (_) {
        // fall through to statement wrapper
      }
    }
  }
  return `(async () => {\n${transformed}\n})()`;
}

async function evalCode(jsCode) {
  try {
    const script = new vm.Script(jsCode, { filename: "<stdin>" });
    return script.runInContext(vmCtx);
  } catch (err) {
    if (err instanceof SyntaxError && /\bawait\b/.test(jsCode)) {
      const wrapped = buildAsyncWrapper(jsCode);
      const asyncScript = new vm.Script(wrapped, { filename: "<stdin>" });
      return await asyncScript.runInContext(vmCtx);
    }
    throw err;
  }
}

function formatResult(val) {
  if (typeof val === "string") {
    return util.inspect(val, { colors: false });
  }
  return util.inspect(val, { depth: 2, colors: false });
}

function emitError(err) {
  const name = err && err.name ? String(err.name) : "Error";
  const value = err && err.message !== undefined ? String(err.message) : String(err);
  const tb = err && err.stack ? String(err.stack) : "";
  sendMsg({ type: "error", name, value, traceback: tb });
}

sendMsg({ type: "_cubepod_ready" });

const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
rl.on("line", async (line) => {
  if (!line.trim()) return;
  rl.pause();
  let executionCount = 1;
  try {
    const req = JSON.parse(line);
    if (req.action === "execute") {
      executionCount = req.execution_count || 1;
      const envVars = req.env_vars || {};
      for (const [k, v] of Object.entries(envVars)) {
        process.env[k] = String(v);
      }
      try {
        let code = req.code || "";
        if (req.language === "typescript") {
          code = stripTypeScript(code);
        }
        const res = await evalCode(code);
        outEmitter.flush();
        errEmitter.flush();
        if (res !== undefined) {
          sendMsg({
            type: "result",
            text: formatResult(res),
            png: null,
            html: null,
          });
        }
      } catch (err) {
        outEmitter.flush();
        errEmitter.flush();
        emitError(err);
      }
      sendMsg({ type: "number_of_executions", execution_count: executionCount });
      sendMsg({ type: "_cubepod_done" });
    }
  } catch (err) {
    emitError(err);
    sendMsg({ type: "number_of_executions", execution_count: executionCount });
    sendMsg({ type: "_cubepod_done" });
  } finally {
    rl.resume();
  }
});
"#;

/// Persistent stateful R kernel worker reading line-counted blocks on stdin.
pub const R_RUNNER_SCRIPT: &str = r#"
json_escape <- function(s) {
  s <- gsub("\\\\", "\\\\\\\\", s)
  s <- gsub("\"", "\\\\\"", s)
  s <- gsub("\r", "\\\\r", s)
  s <- gsub("\t", "\\\\t", s)
  s <- gsub("\n", "\\\\n", s)
  s
}

send_msg <- function(raw_json) {
  cat(raw_json, "\n", sep = "")
  flush(stdout())
}

unescape_env <- function(s) {
  s <- gsub("\\\\n", "\n", s)
  s <- gsub("\\\\\\\\", "\\\\", s)
  s
}

user_env <- new.env(parent = globalenv())
con <- file("stdin", "r")
send_msg('{"type":"_cubepod_ready"}')

while (length(hdr <- readLines(con, n = 1, warn = FALSE)) > 0) {
  parts <- strsplit(hdr, " ", fixed = TRUE)[[1]]
  if (length(parts) < 4 || parts[1] != "EXEC") next
  cnt <- as.integer(parts[2])
  n_code <- as.integer(parts[3])
  n_env <- as.integer(parts[4])

  code_lines <- if (n_code > 0) readLines(con, n = n_code, warn = FALSE) else character(0)
  if (n_env > 0) {
    for (i in seq_len(n_env)) {
      kv <- readLines(con, n = 2, warn = FALSE)
      if (length(kv) == 2) {
        args <- list(unescape_env(kv[2]))
        names(args) <- kv[1]
        do.call(Sys.setenv, args)
      }
    }
  }

  code_text <- paste(code_lines, collapse = "\n")
  out_file <- tempfile()
  err_file <- tempfile()
  out_con <- file(out_file, open = "wt")
  err_con <- file(err_file, open = "wt")

  err_obj <- NULL
  res_obj <- NULL

  sink(out_con)
  sink(err_con, type = "message")
  tryCatch({
    exprs <- parse(text = code_text)
    if (length(exprs) > 0) {
      for (idx in seq_along(exprs)) {
        res_obj <- withVisible(eval(exprs[[idx]], envir = user_env))
      }
    }
  }, error = function(e) {
    err_obj <<- e
  })
  sink(type = "message")
  sink()
  close(out_con)
  close(err_con)

  ts <- as.character(round(as.numeric(Sys.time()) * 1000))
  out_lines <- readLines(out_file, warn = FALSE)
  for (l in out_lines) {
    send_msg(sprintf('{"type":"stdout","text":"%s\\n","timestamp":%s}', json_escape(l), ts))
  }
  err_lines <- readLines(err_file, warn = FALSE)
  for (l in err_lines) {
    send_msg(sprintf('{"type":"stderr","text":"%s\\n","timestamp":%s}', json_escape(l), ts))
  }
  unlink(c(out_file, err_file))

  if (!is.null(err_obj)) {
    msg <- json_escape(conditionMessage(err_obj))
    send_msg(sprintf('{"type":"error","name":"RError","value":"%s","traceback":""}', msg))
  } else if (!is.null(res_obj) && isTRUE(res_obj$visible) && !is.null(res_obj$value)) {
    txt <- paste(capture.output(print(res_obj$value)), collapse = "\n")
    send_msg(sprintf('{"type":"result","text":"%s","png":null,"html":null}', json_escape(txt)))
  }

  send_msg(sprintf('{"type":"number_of_executions","execution_count":%d}', cnt))
  send_msg('{"type":"_cubepod_done"}')
}
"#;

/// Persistent Java `jdk.jshell` kernel launched via `java CubepodJavaKernel.java`.
pub const JAVA_RUNNER_SOURCE: &str = r#"
import jdk.jshell.*;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.logging.LogManager;

public class CubepodJavaKernel {
    private static String escapeJson(String s) {
        if (s == null) return "";
        StringBuilder sb = new StringBuilder(s.length() + 8);
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"': sb.append("\\\""); break;
                case '\\': sb.append("\\\\"); break;
                case '\n': sb.append("\\n"); break;
                case '\r': sb.append("\\r"); break;
                case '\t': sb.append("\\t"); break;
                default:
                    if (c < 0x20) {
                        sb.append(String.format("\\u%04x", (int) c));
                    } else {
                        sb.append(c);
                    }
            }
        }
        return sb.toString();
    }

    private static void emitStreamLines(PrintStream realOut, String type, String text) {
        if (text == null || text.isEmpty()) return;
        long ts = System.currentTimeMillis();
        int idx = 0;
        while (idx < text.length()) {
            int nl = text.indexOf('\n', idx);
            String chunk;
            if (nl >= 0) {
                chunk = text.substring(idx, nl + 1);
                idx = nl + 1;
            } else {
                chunk = text.substring(idx);
                idx = text.length();
            }
            realOut.println("{\"type\":\"" + type + "\",\"text\":\"" + escapeJson(chunk) + "\",\"timestamp\":" + ts + "}");
        }
    }

    public static void main(String[] args) throws Exception {
        // Suppress java.util.prefs FileSystemPreferences INFO log lines on stderr.
        LogManager.getLogManager().reset();

        PrintStream realOut = System.out;
        BufferedReader stdinReader = new BufferedReader(new InputStreamReader(System.in, StandardCharsets.UTF_8));
        ByteArrayOutputStream outBuf = new ByteArrayOutputStream();
        ByteArrayOutputStream errBuf = new ByteArrayOutputStream();
        System.setOut(new PrintStream(outBuf, true, StandardCharsets.UTF_8));
        System.setErr(new PrintStream(errBuf, true, StandardCharsets.UTF_8));

        JShell js = JShell.builder().executionEngine("local").build();
        SourceCodeAnalysis sca = js.sourceCodeAnalysis();

        realOut.println("{\"type\":\"_cubepod_ready\"}");
        realOut.flush();

        String line;
        while ((line = stdinReader.readLine()) != null) {
            if (line.trim().isEmpty()) continue;
            String[] parts = line.split(" ", 2);
            long count = Long.parseLong(parts[0]);
            String code = (parts.length > 1 && !parts[1].equals("-"))
                ? new String(Base64.getDecoder().decode(parts[1]), StandardCharsets.UTF_8)
                : "";

            outBuf.reset();
            errBuf.reset();
            String lastVal = null;
            String errName = null;
            String errValue = null;
            String errTrace = "";

            String rem = code;
            while (!rem.trim().isEmpty() && errName == null) {
                SourceCodeAnalysis.CompletionInfo ci = sca.analyzeCompletion(rem);
                if (ci.completeness() == SourceCodeAnalysis.Completeness.UNKNOWN) {
                    errName = "SyntaxError";
                    errValue = "incomplete or invalid Java syntax near: " + ci.source().trim();
                    break;
                }
                String src = ci.source();
                if (src.trim().isEmpty()) break;
                List<SnippetEvent> events = js.eval(src);
                for (SnippetEvent ev : events) {
                    if (ev.status() == Snippet.Status.REJECTED) {
                        StringBuilder diag = new StringBuilder();
                        js.diagnostics(ev.snippet()).forEach(d -> {
                            if (diag.length() > 0) diag.append("\n");
                            diag.append(d.getMessage(Locale.ROOT));
                        });
                        errName = "CompileError";
                        errValue = diag.length() > 0 ? diag.toString() : "snippet rejected";
                        lastVal = null;
                        break;
                    } else if (ev.exception() != null) {
                        JShellException ex = ev.exception();
                        errName = (ex instanceof EvalException)
                            ? ((EvalException) ex).getExceptionClassName()
                            : ex.getClass().getSimpleName();
                        errValue = ex.getMessage() != null ? ex.getMessage() : "";
                        StringWriter sw = new StringWriter();
                        ex.printStackTrace(new PrintWriter(sw));
                        errTrace = sw.toString();
                        lastVal = null;
                        break;
                    } else if (ev.status() == Snippet.Status.VALID && ev.value() != null && !ev.value().isEmpty()) {
                        lastVal = ev.value();
                    } else {
                        lastVal = null;
                    }
                }
                rem = ci.remaining();
            }

            emitStreamLines(realOut, "stdout", outBuf.toString(StandardCharsets.UTF_8));
            emitStreamLines(realOut, "stderr", errBuf.toString(StandardCharsets.UTF_8));

            if (errName != null) {
                realOut.println("{\"type\":\"error\",\"name\":\"" + escapeJson(errName)
                    + "\",\"value\":\"" + escapeJson(errValue)
                    + "\",\"traceback\":\"" + escapeJson(errTrace) + "\"}");
            } else if (lastVal != null) {
                realOut.println("{\"type\":\"result\",\"text\":\"" + escapeJson(lastVal)
                    + "\",\"png\":null,\"html\":null}");
            }

            realOut.println("{\"type\":\"number_of_executions\",\"execution_count\":" + count + "}");
            realOut.println("{\"type\":\"_cubepod_done\"}");
            realOut.flush();
        }
        js.close();
    }
}
"#;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum WorkerProtocol {
    JsonLine,
    BashBase64,
    RLineCount,
    JavaBase64,
}

#[derive(Debug, Clone)]
pub enum WorkerSpawnError {
    NotInstalled { bin: String },
    Other(String),
}

pub struct WorkerProcess {
    pub child: TrackedChild,
    pub stdin: ChildStdin,
    pub reader: BufReader<ChildStdout>,
    protocol: WorkerProtocol,
    temp_dir: Option<PathBuf>,
}

impl Drop for WorkerProcess {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        if let Some(dir) = self.temp_dir.take() {
            let _ = std::fs::remove_dir_all(dir);
        }
    }
}

pub struct ExecutionContext {
    pub id: String,
    pub language: String,
    pub cwd: String,
    pub execution_count: u64,
    pub worker: Option<WorkerProcess>,
}

fn is_executable_file(path: &Path) -> bool {
    if let Ok(meta) = std::fs::metadata(path) {
        if meta.is_file() {
            return (meta.permissions().mode() & 0o111) != 0;
        }
    }
    false
}

/// Resolves a runtime binary from an optional env override, PATH, or candidate absolute paths.
pub fn find_runtime_binary(
    env_var: Option<&str>,
    candidates: &[&str],
    logical_name: &str,
) -> Result<String, WorkerSpawnError> {
    if let Some(var_name) = env_var {
        if let Ok(val) = env::var(var_name) {
            let trimmed = val.trim();
            if !trimmed.is_empty() {
                let p = Path::new(trimmed);
                if trimmed.contains('/') {
                    if is_executable_file(p) {
                        return Ok(trimmed.to_string());
                    }
                    return Err(WorkerSpawnError::NotInstalled {
                        bin: logical_name.to_string(),
                    });
                }
                if let Some(found) = search_in_path(trimmed) {
                    return Ok(found);
                }
                return Err(WorkerSpawnError::NotInstalled {
                    bin: logical_name.to_string(),
                });
            }
        }
    }

    for &cand in candidates {
        if cand.starts_with('/') {
            if is_executable_file(Path::new(cand)) {
                return Ok(cand.to_string());
            }
        } else if let Some(found) = search_in_path(cand) {
            return Ok(found);
        }
    }

    Err(WorkerSpawnError::NotInstalled {
        bin: logical_name.to_string(),
    })
}

fn search_in_path(bin: &str) -> Option<String> {
    let path_os = env::var_os("PATH")?;
    for dir in env::split_paths(&path_os) {
        let full = dir.join(bin);
        if is_executable_file(&full) {
            return Some(full.to_string_lossy().to_string());
        }
    }
    None
}

pub fn find_python() -> String {
    find_runtime_binary(
        Some("PYTHON"),
        &["python3", "python", "/usr/bin/python3", "/usr/bin/python"],
        "python3",
    )
    .unwrap_or_else(|_| "python3".to_string())
}

fn escape_shell_single_quotes(s: &str) -> String {
    s.replace('\'', "'\\''")
}

fn is_valid_shell_ident(k: &str) -> bool {
    let mut chars = k.chars();
    match chars.next() {
        Some(c) if c.is_ascii_alphabetic() || c == '_' => {}
        _ => return false,
    }
    chars.all(|c| c.is_ascii_alphanumeric() || c == '_')
}

fn escape_r_env(s: &str) -> String {
    s.replace('\\', "\\\\").replace('\n', "\\n")
}

impl ExecutionContext {
    pub fn ensure_worker(&mut self) -> Result<&mut WorkerProcess, WorkerSpawnError> {
        if let Some(ref mut w) = self.worker {
            match w.child.try_wait() {
                Ok(None) => return Ok(self.worker.as_mut().unwrap()),
                _ => {
                    self.worker = None;
                }
            }
        }

        let lang = KernelLanguage::parse(&self.language).map_err(WorkerSpawnError::Other)?;
        let _ = std::fs::create_dir_all(&self.cwd);

        let mut temp_dir: Option<PathBuf> = None;
        let (bin_label, protocol, mut cmd) = match lang {
            KernelLanguage::Python => {
                let bin = find_runtime_binary(
                    Some("PYTHON"),
                    &["python3", "python", "/usr/bin/python3", "/usr/bin/python"],
                    "python3",
                )?;
                let mut c = Command::new(&bin);
                c.arg("-u").arg("-c").arg(PYTHON_RUNNER_SCRIPT);
                (bin, WorkerProtocol::JsonLine, c)
            }
            KernelLanguage::Bash => {
                let bin = find_runtime_binary(
                    Some("CUBEPOD_BASH_BIN"),
                    &["/bin/bash", "/usr/bin/bash", "bash"],
                    "bash",
                )?;
                let mut c = Command::new(&bin);
                c.arg("--norc")
                    .arg("--noprofile")
                    .arg("-c")
                    .arg(BASH_RUNNER_SCRIPT);
                (bin, WorkerProtocol::BashBase64, c)
            }
            KernelLanguage::JavaScript | KernelLanguage::TypeScript => {
                let bin = find_runtime_binary(
                    Some("CUBEPOD_NODE_BIN"),
                    &["node", "/usr/bin/node", "/usr/local/bin/node"],
                    "node",
                )?;
                let mut c = Command::new(&bin);
                c.arg("--no-warnings").arg("-e").arg(NODE_RUNNER_SCRIPT);
                (bin, WorkerProtocol::JsonLine, c)
            }
            KernelLanguage::R => {
                let bin = find_runtime_binary(
                    Some("CUBEPOD_R_BIN"),
                    &[
                        "R",
                        "/usr/bin/R",
                        "/usr/local/bin/R",
                        "Rscript",
                        "/usr/bin/Rscript",
                        "/usr/local/bin/Rscript",
                    ],
                    "R",
                )?;
                let is_rscript = Path::new(&bin)
                    .file_name()
                    .and_then(|n| n.to_str())
                    .map(|n| n.starts_with("Rscript"))
                    .unwrap_or(false);
                let mut c = Command::new(&bin);
                if is_rscript {
                    c.arg("--vanilla").arg("-e").arg(R_RUNNER_SCRIPT);
                } else {
                    c.arg("--vanilla")
                        .arg("--quiet")
                        .arg("--slave")
                        .arg("-e")
                        .arg(R_RUNNER_SCRIPT);
                }
                (bin, WorkerProtocol::RLineCount, c)
            }
            KernelLanguage::Java => {
                let java_bin = find_runtime_binary(
                    Some("CUBEPOD_JAVA_BIN"),
                    &["java", "/usr/bin/java", "/usr/local/bin/java"],
                    "java",
                )?;
                // Verify jshell is also installed so jdk.jshell is available.
                let _jshell_bin = find_runtime_binary(
                    Some("CUBEPOD_JSHELL_BIN"),
                    &["jshell", "/usr/bin/jshell", "/usr/local/bin/jshell"],
                    "jshell",
                )?;
                let dir = env::temp_dir().join(format!(
                    "cubepod-java-{}-{}",
                    std::process::id(),
                    self.id.replace('/', "_")
                ));
                std::fs::create_dir_all(&dir).map_err(|e| {
                    WorkerSpawnError::Other(format!("failed to create java kernel dir: {e}"))
                })?;
                let _ = std::fs::set_permissions(&dir, std::fs::Permissions::from_mode(0o755));
                let src_path = dir.join("CubepodJavaKernel.java");
                std::fs::write(&src_path, JAVA_RUNNER_SOURCE).map_err(|e| {
                    WorkerSpawnError::Other(format!("failed to write java kernel source: {e}"))
                })?;
                let _ = std::fs::set_permissions(&src_path, std::fs::Permissions::from_mode(0o644));
                temp_dir = Some(dir);
                let mut c = Command::new(&java_bin);
                c.arg(src_path);
                (java_bin, WorkerProtocol::JavaBase64, c)
            }
        };

        if Path::new(&self.cwd).exists() {
            cmd.current_dir(&self.cwd);
        }
        cmd.stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit());
        configure_command_credentials(&mut cmd, None, None, None).map_err(|e| {
            if let Some(dir) = temp_dir.take() {
                let _ = std::fs::remove_dir_all(dir);
            }
            WorkerSpawnError::Other(format!("failed to configure interpreter credentials: {e}"))
        })?;

        let mut child = match spawn_tracked(&mut cmd) {
            Ok(c) => c,
            Err(e) => {
                if let Some(dir) = temp_dir.take() {
                    let _ = std::fs::remove_dir_all(dir);
                }
                if e.kind() == std::io::ErrorKind::NotFound {
                    return Err(WorkerSpawnError::NotInstalled { bin: bin_label });
                }
                return Err(WorkerSpawnError::Other(format!(
                    "failed to spawn interpreter '{bin_label}': {e}"
                )));
            }
        };

        let stdin = child
            .stdin()
            .take()
            .ok_or_else(|| WorkerSpawnError::Other("failed to capture stdin".to_string()))?;
        let stdout = child
            .stdout()
            .take()
            .ok_or_else(|| WorkerSpawnError::Other("failed to capture stdout".to_string()))?;
        let mut reader = BufReader::new(stdout);

        let mut ready_line = String::new();
        reader.read_line(&mut ready_line).map_err(|e| {
            WorkerSpawnError::Other(format!(
                "failed to read startup handshake from interpreter: {e}"
            ))
        })?;

        if !ready_line.contains("_cubepod_ready") {
            let _ = child.kill();
            let _ = child.wait();
            if let Some(dir) = temp_dir.take() {
                let _ = std::fs::remove_dir_all(dir);
            }
            return Err(WorkerSpawnError::Other(format!(
                "unexpected interpreter handshake: {ready_line}"
            )));
        }

        self.worker = Some(WorkerProcess {
            child,
            stdin,
            reader,
            protocol,
            temp_dir,
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
        let norm_lang = language
            .as_deref()
            .and_then(|l| KernelLanguage::parse(l).ok())
            .unwrap_or(KernelLanguage::Python)
            .canonical_name()
            .to_string();
        let cwd = cwd
            .filter(|c| !c.is_empty())
            .unwrap_or_else(|| config.workdir.clone());

        let ctx = Arc::new(Mutex::new(ExecutionContext {
            id: id.clone(),
            language: norm_lang,
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
        self.get_or_create_default_for_language(KernelLanguage::Python, config)
    }

    pub fn get_or_create_default_for_language(
        &self,
        lang: KernelLanguage,
        config: &DaemonConfig,
    ) -> Arc<Mutex<ExecutionContext>> {
        let default_id = if lang == KernelLanguage::Python {
            "default".to_string()
        } else {
            format!("default-{}", lang.canonical_name())
        };
        if let Some(existing) = self.get(&default_id) {
            return existing;
        }
        self.create(
            Some(default_id),
            Some(lang.canonical_name().to_string()),
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

    let req_lang_opt = json
        .get("language")
        .and_then(|v| v.as_str())
        .map(str::trim)
        .filter(|s| !s.is_empty());

    let parsed_req_lang = match req_lang_opt {
        Some(raw) => match KernelLanguage::parse(raw) {
            Ok(l) => Some(l),
            Err(msg) => {
                let resp = format!(
                    r#"{{"error":"unsupported_language","name":"UnsupportedLanguageError","value":"{}"}}"#,
                    escape_json(&msg)
                );
                write_json(stream, "400 Bad Request", &resp);
                return;
            }
        },
        None => None,
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
        contexts.get_or_create_default_for_language(
            parsed_req_lang.unwrap_or(KernelLanguage::Python),
            config,
        )
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

    execute_in_context(stream, &ctx_arc, code, parsed_req_lang, &env_vars);
}

fn execute_in_context(
    stream: &mut TcpStream,
    ctx_arc: &Arc<Mutex<ExecutionContext>>,
    code: &str,
    req_lang: Option<KernelLanguage>,
    env_vars: &HashMap<String, String>,
) {
    let mut ctx = ctx_arc.lock().unwrap();
    let ctx_lang = KernelLanguage::parse(&ctx.language).unwrap_or(KernelLanguage::Python);
    if let Some(requested) = req_lang {
        if requested != ctx_lang {
            let resp = format!(
                r#"{{"error":"language_mismatch","details":"context '{}' uses '{}', not '{}'"}}"#,
                escape_json(&ctx.id),
                ctx_lang.canonical_name(),
                requested.canonical_name()
            );
            write_json(stream, "400 Bad Request", &resp);
            return;
        }
    }

    ctx.execution_count += 1;
    let count = ctx.execution_count;
    let canonical_lang = ctx_lang.canonical_name().to_string();
    let cwd = ctx.cwd.clone();

    let worker = match ctx.ensure_worker() {
        Ok(w) => w,
        Err(WorkerSpawnError::NotInstalled { bin }) => {
            // For Java, if `java` exists without `jshell`, run per-cell fallback in cwd.
            if ctx_lang == KernelLanguage::Java && bin == "jshell" {
                drop(ctx);
                execute_java_per_cell(stream, &cwd, code, count, env_vars);
                return;
            }
            let msg = format!("runtime '{bin}' is not installed in this sandbox image");
            let error_json = format!(
                r#"{{"type":"error","name":"KernelNotInstalledError","value":"{}","traceback":""}}"#,
                escape_json(&msg)
            );
            let count_json = format!(
                r#"{{"type":"number_of_executions","execution_count":{}}}"#,
                count
            );
            let body = format!("{error_json}\n{count_json}\n");
            write_response(stream, "200 OK", "application/x-ndjson", body.as_bytes());
            return;
        }
        Err(WorkerSpawnError::Other(e)) => {
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

    let write_ok = write_worker_request(worker, &canonical_lang, code, count, env_vars);
    if write_ok.is_err() {
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

fn write_worker_request(
    worker: &mut WorkerProcess,
    canonical_lang: &str,
    code: &str,
    count: u64,
    env_vars: &HashMap<String, String>,
) -> std::io::Result<()> {
    match worker.protocol {
        WorkerProtocol::JsonLine => {
            let mut env_obj_parts = Vec::new();
            for (k, v) in env_vars {
                env_obj_parts.push(format!(r#""{}":"{}""#, escape_json(k), escape_json(v)));
            }
            let env_json = format!("{{{}}}", env_obj_parts.join(","));
            let req_line = format!(
                r#"{{"action":"execute","language":"{}","code":"{}","execution_count":{},"env_vars":{}}}"#,
                escape_json(canonical_lang),
                escape_json(code),
                count,
                env_json
            );
            worker.stdin.write_all(req_line.as_bytes())?;
            worker.stdin.write_all(b"\n")?;
            worker.stdin.flush()
        }
        WorkerProtocol::BashBase64 => {
            let code_b64 = if code.is_empty() {
                "-".to_string()
            } else {
                base64_encode(code.as_bytes())
            };
            let mut env_script = String::new();
            for (k, v) in env_vars {
                if is_valid_shell_ident(k) {
                    env_script
                        .push_str(&format!("export {k}='{}'\n", escape_shell_single_quotes(v)));
                }
            }
            let env_b64 = if env_script.is_empty() {
                "-".to_string()
            } else {
                base64_encode(env_script.as_bytes())
            };
            let line = format!("{count} {code_b64} {env_b64}\n");
            worker.stdin.write_all(line.as_bytes())?;
            worker.stdin.flush()
        }
        WorkerProtocol::RLineCount => {
            let code_lines: Vec<&str> = if code.is_empty() {
                Vec::new()
            } else {
                code.split('\n').collect()
            };
            let hdr = format!("EXEC {count} {} {}\n", code_lines.len(), env_vars.len());
            worker.stdin.write_all(hdr.as_bytes())?;
            for l in code_lines {
                worker.stdin.write_all(l.as_bytes())?;
                worker.stdin.write_all(b"\n")?;
            }
            for (k, v) in env_vars {
                worker.stdin.write_all(k.as_bytes())?;
                worker.stdin.write_all(b"\n")?;
                worker.stdin.write_all(escape_r_env(v).as_bytes())?;
                worker.stdin.write_all(b"\n")?;
            }
            worker.stdin.flush()
        }
        WorkerProtocol::JavaBase64 => {
            let code_b64 = if code.is_empty() {
                "-".to_string()
            } else {
                base64_encode(code.as_bytes())
            };
            let line = format!("{count} {code_b64}\n");
            worker.stdin.write_all(line.as_bytes())?;
            worker.stdin.flush()
        }
    }
}

/// Per-cell `java` source-file launcher fallback when `java` is installed without `jshell`.
fn execute_java_per_cell(
    stream: &mut TcpStream,
    cwd: &str,
    code: &str,
    count: u64,
    env_vars: &HashMap<String, String>,
) {
    let java_bin = match find_runtime_binary(
        Some("CUBEPOD_JAVA_BIN"),
        &["java", "/usr/bin/java", "/usr/local/bin/java"],
        "java",
    ) {
        Ok(b) => b,
        Err(_) => {
            let error_json = r#"{"type":"error","name":"KernelNotInstalledError","value":"runtime 'java' is not installed in this sandbox image","traceback":""}"#;
            let count_json = format!(
                r#"{{"type":"number_of_executions","execution_count":{}}}"#,
                count
            );
            let body = format!("{error_json}\n{count_json}\n");
            write_response(stream, "200 OK", "application/x-ndjson", body.as_bytes());
            return;
        }
    };

    let header =
        "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: close\r\n\r\n";
    if stream.write_all(header.as_bytes()).is_err() || stream.flush().is_err() {
        return;
    }

    let _ = std::fs::create_dir_all(cwd);
    let src_dir = env::temp_dir().join(format!("cubepod-java-cell-{}-{count}", std::process::id()));
    let _ = std::fs::create_dir_all(&src_dir);
    let _ = std::fs::set_permissions(&src_dir, std::fs::Permissions::from_mode(0o755));
    let src_file = src_dir.join("Main.java");
    let full_src = if code.contains("class ") {
        code.to_string()
    } else {
        format!("public class Main {{\n  public static void main(String[] args) throws Exception {{\n    {code}\n  }}\n}}\n")
    };
    if let Err(e) = std::fs::write(&src_file, full_src) {
        let err_msg = format!(
            r#"{{"type":"error","name":"IoError","value":"{}","traceback":""}}"#,
            escape_json(&e.to_string())
        );
        let exec_cnt = format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
        let _ = stream.write_all(format!("{err_msg}\n{exec_cnt}\n").as_bytes());
        let _ = stream.flush();
        let _ = std::fs::remove_dir_all(&src_dir);
        return;
    }
    let _ = std::fs::set_permissions(&src_file, std::fs::Permissions::from_mode(0o644));

    let mut cmd = Command::new(&java_bin);
    cmd.arg(&src_file);
    if Path::new(cwd).exists() {
        cmd.current_dir(cwd);
    }
    if let Err(e) = configure_command_credentials(&mut cmd, None, None, None) {
        let err_msg = format!(
            r#"{{"type":"error","name":"CredentialError","value":"{}","traceback":""}}"#,
            escape_json(&e)
        );
        let exec_cnt = format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
        let _ = stream.write_all(format!("{err_msg}\n{exec_cnt}\n").as_bytes());
        let _ = stream.flush();
        let _ = std::fs::remove_dir_all(&src_dir);
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

    if let Ok(mut child) = spawn_tracked(&mut cmd) {
        if let Some(out) = child.stdout().take() {
            for line in BufReader::new(out).lines().map_while(Result::ok) {
                let msg = format!(
                    r#"{{"type":"stdout","text":"{}\n","timestamp":{}}}"#,
                    escape_json(&line),
                    now_ms()
                );
                let _ = stream.write_all(format!("{msg}\n").as_bytes());
            }
        }
        if let Some(err) = child.stderr().take() {
            for line in BufReader::new(err).lines().map_while(Result::ok) {
                let msg = format!(
                    r#"{{"type":"stderr","text":"{}\n","timestamp":{}}}"#,
                    escape_json(&line),
                    now_ms()
                );
                let _ = stream.write_all(format!("{msg}\n").as_bytes());
            }
        }
        let exit_code = child
            .wait()
            .map(|s| exit_code_from_status(&s))
            .unwrap_or(-1);
        if exit_code != 0 {
            let err_msg = format!(
                r#"{{"type":"error","name":"ProcessExitError","value":"process exited with code {exit_code}","traceback":""}}"#
            );
            let _ = stream.write_all(format!("{err_msg}\n").as_bytes());
        }
    } else {
        let err_msg = r#"{"type":"error","name":"KernelNotInstalledError","value":"runtime 'java' is not installed in this sandbox image","traceback":""}"#;
        let _ = stream.write_all(format!("{err_msg}\n").as_bytes());
    }

    let _ = std::fs::remove_dir_all(&src_dir);
    let exec_cnt = format!(r#"{{"type":"number_of_executions","execution_count":{count}}}"#);
    let _ = stream.write_all(format!("{exec_cnt}\n").as_bytes());
    let _ = stream.flush();
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

    if let Some(ref raw_lang) = language {
        if !raw_lang.trim().is_empty() {
            if let Err(msg) = KernelLanguage::parse(raw_lang) {
                let resp = format!(
                    r#"{{"error":"unsupported_language","name":"UnsupportedLanguageError","value":"{}"}}"#,
                    escape_json(&msg)
                );
                write_json(stream, "400 Bad Request", &resp);
                return;
            }
        }
    }

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
