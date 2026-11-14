#!/usr/bin/env bash
# 打包：生成 dist/tidb-deploy-<版本>.tar.gz（强制 LF 换行并校验，保证能在 CentOS 上运行）
set -e
HERE="$(cd "$(dirname "$0")" && pwd)"
VER="$(grep -m1 '^TD_VERSION=' "$HERE/tidb-deploy.sh" | cut -d'"' -f2)"
NAME="tidb-deploy-${VER}"
STAGE="$(mktemp -d)/${NAME}"
mkdir -p "$STAGE/tests/fixtures" "$HERE/dist"

cp "$HERE/tidb-deploy.sh" "$HERE/deploy.conf.example" "$HERE/README.md" "$STAGE/"
cp "$HERE/tests/run-tests.sh" "$STAGE/tests/"
cp "$HERE"/tests/fixtures/os-release.* "$STAGE/tests/fixtures/"

# 统一为 LF（Windows 上编辑可能引入 CRLF，CRLF 会导致 "bad interpreter: /usr/bin/env bash^M"）
python - "$STAGE" <<'PY'
import os, sys
bad = []
for root, _, files in os.walk(sys.argv[1]):
    for f in files:
        p = os.path.join(root, f)
        b = open(p, 'rb').read()
        if b'\r' in b:
            open(p, 'wb').write(b.replace(b'\r\n', b'\n').replace(b'\r', b'\n'))
        if b'\r' in open(p, 'rb').read():
            bad.append(p)
if bad:
    print('仍含 CR:', bad); sys.exit(1)
print('换行符检查通过：全部为 LF')
PY

chmod +x "$STAGE/tidb-deploy.sh" "$STAGE/tests/run-tests.sh"
bash -n "$STAGE/tidb-deploy.sh"
tar czf "$HERE/dist/${NAME}.tar.gz" -C "$(dirname "$STAGE")" "$NAME"
( cd "$HERE/dist" && sha256sum "${NAME}.tar.gz" | tee "${NAME}.tar.gz.sha256" )
echo "已生成: $HERE/dist/${NAME}.tar.gz"
