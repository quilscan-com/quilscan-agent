#!/usr/bin/env python3
"""只复制 State 源码到临时目录；不启动 Agent，不连接真实服务。

用法：python3 repro-agent-state.py --source /path/to/quilscan-agent
退出 0 表示已复现缺陷；修复后必须转换为正确行为回归断言。
"""
import argparse
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--source", type=Path, default=Path(__file__).resolve().parents[2])
args = parser.parse_args()
source = args.source.resolve()
if not (source / "internal/config/state.go").is_file():
    parser.error("源码不存在；请用 --source 指定 quilscan-com/quilscan-agent 的本地 checkout")
revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip()
print("Source revision:", revision, flush=True)
env = dict(os.environ, GOWORK="off")
with tempfile.TemporaryDirectory(prefix="quilscan-state-repro-") as directory:
    scratch = Path(directory)
    (scratch / "internal/config").mkdir(parents=True)
    for relative in ("go.mod", "go.sum", "internal/config/state.go", "internal/config/config.go"):
        shutil.copy2(source / relative, scratch / relative)
    (scratch / "internal/config/state_audit_test.go").write_text(r'''package config
import ("path/filepath"; "testing")
func TestAuditStaleSnapshotLosesNodeVersion(t *testing.T) {
 p := filepath.Join(t.TempDir(), "state.yaml")
 if err := SaveState(p, &State{ConfigPath:"/node", NodeVersion:"old"}); err != nil {t.Fatal(err)}
 stale, err := LoadState(p); if err != nil {t.Fatal(err)}
 fresh, err := LoadState(p); if err != nil {t.Fatal(err)}
 fresh.NodeVersion = "new"
 if err := SaveState(p, fresh); err != nil {t.Fatal(err)}
 stale.PeerID = "observed-peer"
 if err := SaveState(p, stale); err != nil {t.Fatal(err)}
 actual, err := LoadState(p); if err != nil {t.Fatal(err)}
 if actual.NodeVersion != "new" {t.Fatalf("stale reconciliation overwrote installed version: got %q, want new", actual.NodeVersion)}
}
''')
    result = subprocess.run(["go", "test", "-race", "./internal/config"], cwd=scratch, env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=180)
    print(result.stdout, end="")
    if result.returncode != 0 and 'stale reconciliation overwrote installed version: got "old", want new' in result.stdout:
        print("CONFIRMED: exact-source State stale-save defect reproduced")
    else:
        raise SystemExit("NOT REPRODUCED: inspect output; failure may be environmental, or source may be fixed")
