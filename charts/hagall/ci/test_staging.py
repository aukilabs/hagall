"""Offline rendered-manifest contract tests. Requires Helm 3 and PyYAML.

CI values are test fixtures only, never staging deployment inputs.
"""
import copy
import pathlib
import subprocess
import tempfile
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parents[3]
CHART = ROOT / "charts/hagall"
BASE = "f51fbf835936275b95c731ab7c7d70e68f21053b"
DIGEST = "sha256:72550a08119d48e39b3d34d4e1a815624871a090a0b8a24b8f88ba2bcb00ba24"


def helm(chart=CHART, overlays=(), extra=(), lint=False, ok=True):
    args = (["helm", "lint", "--strict", str(chart)] if lint else
            ["helm", "template", "auki-relay-node", str(chart), "--namespace", "default"])
    for overlay in overlays:
        args += ["-f", str(chart / overlay)]
    result = subprocess.run(args + list(extra), capture_output=True, text=True)
    if ok and result.returncode:
        raise AssertionError(result.stderr + result.stdout)
    return result


def render(active=False):
    overlays = ["values.staging.yaml", "ci/values.yaml"]
    if active:
        overlays.append("values.staging-active.yaml")
    return list(filter(None, yaml.safe_load_all(helm(overlays=overlays).stdout)))


def deployment(docs):
    return next(d for d in docs if d["kind"] == "Deployment")


class StagingContract(unittest.TestCase):
    def test_lint_preflight_and_activation(self):
        for active in (False, True):
            files = ["values.staging.yaml", "ci/values.yaml"]
            if active:
                files.append("values.staging-active.yaml")
            helm(overlays=files, lint=True)

    def test_effective_startup_and_identity(self):
        docs = render()
        self.assertFalse(any(d["kind"] in ("Secret", "Ingress", "PodMonitor") for d in docs))
        self.assertTrue(all(d["metadata"]["name"].startswith("auki-relay-node") for d in docs))
        dep = deployment(docs)
        self.assertEqual(dep["spec"]["replicas"], 1)
        self.assertEqual(dep["spec"]["strategy"], {"type": "Recreate"})
        pod = dep["spec"]["template"]["spec"]
        for c in pod["containers"] + pod["initContainers"]:
            self.assertEqual(c["image"], "aukilabs/hagall@" + DIGEST)
        env = {e["name"]: e["value"] for e in pod["containers"][0]["env"]}
        self.assertEqual(env["RELAY_ACCEPT_BOOKINGS"], "false")
        for key in ("DDS_MAX_CONCURRENCY", "LOCAL_CAPACITY", "MAX_RESERVATIONS",
                    "MAX_RESERVATIONS_PER_IP", "MAX_RESERVATIONS_PER_ASN", "MAX_CIRCUITS_PER_PEER"):
            self.assertEqual(env["RELAY_" + key], "128")
        fixtures = yaml.safe_load((CHART / "ci/values.yaml").read_text())["hagall"]
        for key, value in (("DDS_URL", "ddsUrl"), ("DMS_URL", "dmsUrl"),
                           ("DDS_PUBLIC_KEY_URL", "ddsPublicKeyUrl")):
            self.assertEqual(env["RELAY_" + key], fixtures["relay"][value])
        host, peer = fixtures["relay"]["publicHost"], fixtures["relay"]["peerId"]
        self.assertEqual(env["RELAY_PUBLIC_BASE_MULTIADDRS"],
                         f"/dns4/{host}/tcp/443/p2p/{peer},/dns4/{host}/tcp/4443/wss/p2p/{peer}")
        self.assertEqual(env["RELAY_ADMIN_ADDR"], "0.0.0.0:9090")
        self.assertEqual(env["RELAY_METRICS_ADDR"], "0.0.0.0:9091")
        source = next(v for v in pod["volumes"] if v["name"] == "identity-source")
        self.assertEqual(source["secret"]["secretName"], fixtures["identity"]["existingSecret"])
        self.assertEqual(dep["spec"]["selector"]["matchLabels"],
                         {"app.kubernetes.io/name": "auki-relay-node"})

    def test_network_boundary(self):
        services = {d["metadata"]["name"]: d for d in render() if d["kind"] == "Service"}
        self.assertEqual(set(services), {"auki-relay-node-public", "auki-relay-node-admin"})
        public = services["auki-relay-node-public"]
        self.assertEqual(public["spec"]["type"], "LoadBalancer")
        self.assertEqual(public["spec"]["loadBalancerClass"], "service.k8s.aws/nlb")
        self.assertEqual([(p["port"], p["targetPort"]) for p in public["spec"]["ports"]],
                         [(443, "relay"), (4443, "relay-wss")])
        annotations = public["metadata"]["annotations"]
        prefix = "service.beta.kubernetes.io/aws-load-balancer-"
        self.assertEqual(annotations[prefix + "ssl-ports"], "relay-wss")
        self.assertEqual(annotations[prefix + "backend-protocol"], "tcp")
        fixtures = yaml.safe_load((CHART / "ci/values.yaml").read_text())["hagall"]["service"]["public"]
        self.assertEqual(annotations[prefix + "ssl-cert"], fixtures["certificateArn"])
        self.assertEqual(annotations[prefix + "subnets"], ",".join(fixtures["subnetIds"]))
        private = services["auki-relay-node-admin"]["spec"]
        self.assertEqual(private["type"], "ClusterIP")
        self.assertEqual([p["port"] for p in private["ports"]], [9090, 9091])

    def test_activation_changes_only_booking_gate(self):
        before, after = render(), render(active=True)
        expected = copy.deepcopy(before)
        env = deployment(expected)["spec"]["template"]["spec"]["containers"][0]["env"]
        next(e for e in env if e["name"] == "RELAY_ACCEPT_BOOKINGS")["value"] = "true"
        self.assertEqual(after, expected)

    def test_missing_infrastructure_fails_closed(self):
        result = helm(overlays=["values.staging.yaml"], ok=False)
        self.assertNotEqual(result.returncode, 0)
        for field in ("existingSecret", "ddsUrl", "peerId", "subnetIds", "certificateArn"):
            self.assertIn(field, result.stderr)

    def test_shared_identity_and_capacity_guards(self):
        for setting in ("hagall.replicaCount=2", "hagall.relay.capacity=257"):
            result = helm(overlays=["values.staging.yaml", "ci/values.yaml"],
                          extra=["--set", setting], ok=False)
            self.assertNotEqual(result.returncode, 0)

    def test_base_and_dev_effective_manifests_unchanged(self):
        # Reconstruct pre-change wrapper from immutable source, reusing the same
        # locked dependency. This is a manifest comparison, not just a YAML diff.
        with tempfile.TemporaryDirectory() as temp:
            old = pathlib.Path(temp)
            for name in ("Chart.yaml", "Chart.lock", "values.yaml", "values.dev.yaml", "ci/values.yaml"):
                target = old / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(subprocess.check_output(
                    ["git", "show", f"{BASE}:charts/hagall/{name}"], cwd=ROOT))
                self.assertEqual(target.read_bytes(), (CHART / name).read_bytes())
            (old / "charts").symlink_to(CHART / "charts", target_is_directory=True)
            for overlays in (["ci/values.yaml"], ["ci/values.yaml", "values.dev.yaml"]):
                self.assertEqual(helm(chart=old, overlays=overlays).stdout,
                                 helm(overlays=overlays).stdout)
        # There is no production overlay in this wrapper. Base equality proves
        # non-target defaults unchanged, not that any live production was tested.
        self.assertFalse((CHART / "values.prod.yaml").exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
