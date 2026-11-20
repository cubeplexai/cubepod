"""Regression checks for Helm panel key generation, wiring and rollouts."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import shutil
import tempfile
import threading
from pathlib import Path
import re
import subprocess
import unittest

CHART = Path(__file__).parent / "cubepod"


def render(*values):
    args = ["helm", "template", "panel-test", str(CHART)]
    for value in values:
        args += ["--set-string", value]
    return subprocess.check_output(args, text=True)


def generated_secret(manifest):
    match = re.search(r"^  panel-secret: (\S+)$", manifest, re.MULTILINE)
    return base64.b64decode(match[1]) if match else None


class PanelSecretChartTests(unittest.TestCase):
    def test_generated_random_key_without_rollout_noise(self):
        first, second = render(), render()
        self.assertEqual(len(generated_secret(first)), 64)
        self.assertNotEqual(generated_secret(first), generated_secret(second))
        for result in (first, second):
            self.assertNotIn("checksum/panel-secret", result)
            self.assertIn("name: panel-test-cubepod-panel-secret", result)
            self.assertRegex(result, r"name: CUBEPOD_PANEL_SECRET\s+valueFrom:\s+secretKeyRef:\s+name: panel-test-cubepod-panel-secret\s+key: panel-secret")

    def test_explicit_key_and_stable_rollout_checksum(self):
        first = render("gateway.panelSecret=operator-test-key")
        second = render("gateway.panelSecret=operator-test-key")
        changed = render("gateway.panelSecret=rotated-test-key")
        self.assertEqual(generated_secret(first), b"operator-test-key")
        self.assertEqual(first, second)
        checksum = lambda m: re.search(r'checksum/panel-secret: "([a-f0-9]{64})"', m)[1]
        self.assertNotEqual(checksum(first), checksum(changed))

    def test_external_secret_precedence_and_deterministic_render(self):
        values = ["gateway.existingPanelSecret=external-panel", "gateway.panelSecret=ignored"]
        first = render(*values)
        self.assertEqual(first, render(*values))
        self.assertIsNone(generated_secret(first))
        self.assertNotIn("checksum/panel-secret", first)
        self.assertRegex(first, r"name: CUBEPOD_PANEL_SECRET\s+valueFrom:\s+secretKeyRef:\s+name: external-panel\s+key: panel-secret")

    def test_online_lookup_reuses_existing_key(self):
        existing = base64.b64encode(b"persisted-online-test-key").decode()
        seen = []

        class API(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                path = self.path.split("?", 1)[0]
                seen.append(path)
                responses = {
                    "/version": {"major": "1", "minor": "31", "gitVersion": "v1.31.0"},
                    "/api": {"kind": "APIVersions", "apiVersion": "v1", "versions": ["v1"]},
                    "/apis": {"kind": "APIGroupList", "apiVersion": "v1", "groups": []},
                    "/api/v1": {"kind": "APIResourceList", "groupVersion": "v1", "resources": [{"name": "secrets", "singularName": "secret", "namespaced": True, "kind": "Secret", "verbs": ["get", "list"]}]},
                    "/api/v1/namespaces/default/secrets/panel-test-cubepod-panel-secret": {
                    "kind": "Secret",
                    "apiVersion": "v1",
                    "metadata": {
                        "name": "panel-test-cubepod-panel-secret",
                        "namespace": "default",
                        "labels": {"app.kubernetes.io/managed-by": "Helm"},
                        "annotations": {
                            "meta.helm.sh/release-name": "panel-test",
                            "meta.helm.sh/release-namespace": "default",
                        },
                    },
                    "data": {"panel-secret": existing},
                },
                }
                response = responses.get(path)
                self.send_response(200 if response is not None else 404)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(response or {"kind": "Status", "code": 404, "reason": "NotFound"}).encode())

        server = ThreadingHTTPServer(("127.0.0.1", 0), API)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as tmp:
                fixture = Path(tmp) / "chart"
                (fixture / "templates").mkdir(parents=True)
                for name in ("Chart.yaml", "values.yaml", "templates/_helpers.tpl", "templates/panel-secret.yaml"):
                    shutil.copyfile(CHART / name, fixture / name)
                kubeconfig = Path(tmp) / "config"
                kubeconfig.write_text(json.dumps({"apiVersion": "v1", "kind": "Config", "clusters": [{"name": "fixture", "cluster": {"server": "http://127.0.0.1:"+str(server.server_port)}}], "contexts": [{"name": "fixture", "context": {"cluster": "fixture", "user": "fixture"}}], "current-context": "fixture", "users": [{"name": "fixture", "user": {}}]}))
                kubeconfig.chmod(0o600)
                # Helm 4 --dry-run=server also downloads the cluster OpenAPI
                # schema; this fixture only serves lookup. Skip schema validation
                # so the test exercises Secret reuse without a full apiserver.
                rendered = subprocess.run(
                    [
                        "helm",
                        "template",
                        "panel-test",
                        str(fixture),
                        "--dry-run=server",
                        "--disable-openapi-validation",
                        "--kubeconfig",
                        str(kubeconfig),
                    ],
                    capture_output=True,
                    text=True,
                    timeout=20,
                )
                if rendered.returncode != 0:
                    self.fail(rendered.stderr or rendered.stdout)
                result = rendered.stdout
                self.assertEqual(generated_secret(result), b"persisted-online-test-key")
                self.assertTrue(any("/secrets/panel-test-cubepod-panel-secret" in p for p in seen))
        finally:
            server.shutdown()
            server.server_close()
            thread.join()


if __name__ == "__main__":
    unittest.main()
