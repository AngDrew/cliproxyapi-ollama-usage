#!/usr/bin/env python3
"""Real plugin + real CPA + fake HTTPS Ollama, on a dedicated test network.
Run: python3 tests/integration.py [--image eceasy/cli-proxy-api:latest]
Requires Docker, openssl, and dist/ollama-usage.so built for the Docker host.
No real credentials, inference, or Ollama requests.
"""
import argparse
import json
import pathlib
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--image", default="eceasy/cli-proxy-api:latest")
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parents[1]
plugin = root / "dist/ollama-usage.so"
assert plugin.is_file(), "Build the plugin first (see README)"
name = "ollama-usage-check-" + secrets.token_hex(5)
fixture = name + "-fixture"
network = name + "-network"
password = secrets.token_hex(24)
def docker(*cmd):
    return subprocess.check_output(["docker", *cmd], text=True).strip()

with tempfile.TemporaryDirectory(prefix="ollama-usage-check-") as temporary:
    folder = pathlib.Path(temporary)
    (folder / "auths").mkdir()
    monitors = {
        "missing": {"api_key_env": "OLLAMA_TEST_MISSING_KEY"},
        "credits": {"api_key": "fixture-key"},
        "legacy": {"api_key": "legacy-key"},
        "denied": {"api_key": "invalid-key"},
        "limited": {"api_key": "rate-limited"},
    }
    for key, fields in monitors.items():
        (folder / f"auths/ollama-{key}.json").write_text(json.dumps({"type": "ollama-usage", **fields}))
    (folder / "config.yaml").write_text(f"""
host: ""
port: 8317
auth-dir: /test/auths
api-keys: ["inference-test-key"]
remote-management:
  allow-remote: true
  secret-key: "{password}"
  disable-control-panel: true
plugins:
  enabled: true
  dir: /test/plugins
  configs:
    ollama-usage:
      enabled: true
""")
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-keyout", str(folder / "ca.key"), "-out", str(folder / "ca.crt"),
                    "-subj", "/CN=ollama.com", "-addext", "subjectAltName=DNS:ollama.com"],
                   check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        docker("network", "create", network)
        docker("run", "-d", "--name", fixture, "--network", network, "--network-alias", "ollama.com",
               "-v", str(folder) + ":/test:ro", "-v", str(root / "tests/fixture.go") + ":/fixture.go:ro",
               "golang:1.26.10-bookworm", "go", "run", "/fixture.go")
        for attempt in range(200):
            if "fixture ready" in docker("logs", fixture):
                break
            time.sleep(0.1)
        else:
            raise AssertionError("HTTPS fixture did not start")
        docker("run", "-d", "--name", name, "--network", network, "-p", "127.0.0.1::8317",
               "-e", "SSL_CERT_FILE=/test/ca.crt",
               "-v", str(folder) + ":/test", "-v", str(plugin.parent) + ":/test/plugins:ro",
               args.image, "./CLIProxyAPI", "-config", "/test/config.yaml", "-local-model")
        address = docker("port", name, "8317/tcp").splitlines()[0]
        base = "http://" + address
        def request(path, body=None, authenticated=True):
            data = None if body is None else json.dumps(body).encode()
            headers = {"Content-Type": "application/json"}
            if authenticated:
                headers["Authorization"] = "Bearer " + password
            req = urllib.request.Request(base + path, data=data, headers=headers)
            try:
                with urllib.request.urlopen(req, timeout=30) as response:
                    return response.status, json.load(response)
            except urllib.error.HTTPError as error:
                return error.code, json.load(error)
        for attempt in range(100):
            try:
                status, listed = request("/v0/management/auth-files")
                if status == 200 and len(listed["files"]) == len(monitors):
                    break
            except (OSError, ValueError):
                pass
            time.sleep(0.1)
        else:
            raise AssertionError("CLIProxyAPI did not become ready")
        credentials = {file["name"]: file for file in listed["files"]}
        for file in credentials.values():
            assert file["provider"] == "ollama-usage", file
            assert file["supports_quota"] is True, file
            assert file["quota_provider"] == "ollama-usage", file
        assert request("/v0/management/auth-files", authenticated=False)[0] == 401
        status, providers = request("/v0/management/quota/providers")
        assert status == 200 and any(p["plugin_id"] == "ollama-usage" for p in providers["providers"]), providers
        status, plugins = request("/v8/management/plugins")
        assert status == 200, plugins
        assert any(p["id"] == "ollama-usage" and p["supports_quota"] for p in plugins["plugins"]), plugins
        status, v8 = request("/v8/management/credentials")
        assert status == 200 and all(f["quota_plugin_id"] == "ollama-usage" for f in v8["files"]), v8
        for path in ["/v0/management/quota/fetch", "/v8/management/plugins/ollama-usage/quota"]:
            for kind in monitors:
                payload = {"auth_index": credentials[f"ollama-{kind}.json"]["auth_index"]}
                status, response = request(path, payload)
                if kind in ["credits", "legacy"]:
                    assert status == 200, (path, kind, status, response)
                    expected = 0.725 if kind == "credits" else 0.75
                    assert response["groups"][0]["buckets"][0]["remainingFraction"] == expected, response
                    summary = {m["key"]: m["value"] for m in response["summary"]}
                    assert summary["purchased_balance_usd"] == 25 and summary["request_count_24h"] == 15, response
                    if kind == "credits":
                        assert summary["usage_usd_24h"] == 0.01718, response
                    else:
                        assert "usage_usd_24h" not in summary and "input_tokens_24h" not in summary, response
                    assert "fixture-key" not in json.dumps(response), response
                else:
                    phrase = {"missing": "API key is missing", "denied": "HTTP 401", "limited": "120 seconds"}[kind]
                    assert status == 502 and phrase in json.dumps(response), (path, kind, status, response)
                    assert "PRIVATE-DO-NOT-ECHO" not in json.dumps(response), response
        print("PASS: real .so + CPA, Plugin-tab discovery, v0/v8 quotas, credits/legacy balance, usage, management auth and sanitized 401/429; fake HTTPS Ollama only")
    except Exception:
        for container in [name, fixture]:
            subprocess.run(["docker", "logs", container], check=False)
        raise
    finally:
        for container in [name, fixture]:
            subprocess.run(["docker", "rm", "-f", container], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        subprocess.run(["docker", "network", "rm", network], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
