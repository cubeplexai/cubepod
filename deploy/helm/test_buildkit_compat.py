"""Regression checks for BuildKit Kubernetes version compatibility (AppArmor < 1.30 vs >= 1.30)."""
from pathlib import Path
import subprocess
import unittest

CHART = Path(__file__).parent / "cubepod"


def render_buildkit(kube_version=None, replicas=1):
    args = [
        "helm",
        "template",
        "buildkit-test",
        str(CHART),
        "--set",
        "templateBuild.enabled=true",
        "--set",
        f"templateBuild.buildkit.replicaCount={replicas}",
        "--show-only",
        "templates/buildkit.yaml",
    ]
    if kube_version:
        args += ["--kube-version", kube_version]
    return subprocess.check_output(args, text=True)


class BuildKitCompatChartTests(unittest.TestCase):
    def test_pre_130_uses_beta_apparmor_annotation_without_field(self):
        for kube_ver in ("1.27.8", "1.29.4"):
            for replicas in (1, 2):
                rendered = render_buildkit(kube_version=kube_ver, replicas=replicas)
                self.assertIn(
                    "container.apparmor.security.beta.kubernetes.io/buildkitd: unconfined",
                    rendered,
                    f"expected AppArmor beta annotation for KubeVersion {kube_ver} replicas={replicas}",
                )
                self.assertNotIn(
                    "appArmorProfile:",
                    rendered,
                    f"unexpected GA appArmorProfile for KubeVersion {kube_ver} replicas={replicas}",
                )

    def test_130_plus_uses_ga_apparmor_profile_field(self):
        for kube_ver in (None, "1.30.0", "1.31.2"):
            for replicas in (1, 2):
                rendered = render_buildkit(kube_version=kube_ver, replicas=replicas)
                self.assertIn(
                    "appArmorProfile:",
                    rendered,
                    f"expected GA appArmorProfile for KubeVersion {kube_ver} replicas={replicas}",
                )
                self.assertNotIn(
                    "container.apparmor.security.beta.kubernetes.io/buildkitd",
                    rendered,
                    f"unexpected beta AppArmor annotation for KubeVersion {kube_ver} replicas={replicas}",
                )


if __name__ == "__main__":
    unittest.main()
