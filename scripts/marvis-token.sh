#!/bin/bash
# 从本机已登录的 Marvis（仅 macOS）打印 access_token 和 refresh_token。
# 导出的 token 只输出到终端；必要的临时 Cookie 副本会自动删除。
# 系统可能会询问是否允许读取钥匙串。不要把输出贴到 issue 或日志。
# 把 name / access_token / refresh_token / openid / guid 填进面板「账号」的手工填写。
set +x
set -euo pipefail

if [ "$(uname -s)" != "Darwin" ]; then
  echo "这个命令只在 macOS 上读取本机 Marvis。其他系统请在客户端登录后自行取得 access_token，再在面板里添加账号。" >&2
  exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
  echo "需要 python3。" >&2
  exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
  echo "需要 openssl。" >&2
  exit 1
fi
if ! command -v security >/dev/null 2>&1; then
  echo "需要 macOS 的 security 命令。" >&2
  exit 1
fi

pw="$(security find-generic-password -s "Marvis Safe Storage" -a "Marvis Key" -w 2>/dev/null || true)"
if [ -z "$pw" ]; then
  echo "读不到钥匙串「Marvis Safe Storage」。请在系统弹窗里点允许后重试。" >&2
  exit 1
fi

MARVIS_SAFE_PW="$pw" python3 - <<'PY'
import hashlib, os, sqlite3, subprocess, sys, tempfile, urllib.parse
from pathlib import Path

def fail(msg):
    print(msg, file=sys.stderr)
    sys.exit(1)

pw = os.environ.get("MARVIS_SAFE_PW", "").encode()
pw = pw.rstrip(b"\r\n")
if not pw:
    fail("钥匙串「Marvis Safe Storage」是空的")

home = Path.home()
cookie = home / "Library" / "Application Support" / "com.tencent.mac.marvis" / "Cookies"
if not cookie.is_file():
    fail("没有找到本机 Marvis 的 Cookie 文件。请先打开 Marvis 并登录")

def load_rows(path):
    uri = Path(path).resolve().as_uri() + "?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        return con.execute(
            "SELECT host_key, name, encrypted_value, value FROM cookies "
            "WHERE name IN ('accesstoken','refreshtoken','openid','nickname','guid')"
        ).fetchall()
    finally:
        con.close()

try:
    rows = load_rows(cookie)
except sqlite3.OperationalError:
    with tempfile.TemporaryDirectory(prefix="marvis-cookies-") as temp_dir:
        tmp = Path(temp_dir)
        dest = tmp / "Cookies"
        dest.write_bytes(cookie.read_bytes())
        journal = cookie.with_name("Cookies-journal")
        if journal.is_file():
            (tmp / "Cookies-journal").write_bytes(journal.read_bytes())
        rows = load_rows(dest)

picked = {}
for host, name, blob, plain in rows:
    host = host or ""
    prev = picked.get(name)
    if prev is None or ("qq.com" in host and "qq.com" not in prev[0]):
        picked[name] = (host, blob, plain)

def pkcs7(raw):
    if not raw:
        fail("解开 Cookie 失败")
    n = raw[-1]
    if n == 0 or n > 16 or raw[-n:] != bytes([n]) * n:
        fail("解开 Cookie 失败")
    return raw[:-n]

def plaintext(raw):
    if len(raw) >= 32 and raw[11] == 0:
        raw = raw[32:]
    return raw.rstrip(b"\x00").decode("utf-8", "replace")

def decrypt(blob, plain):
    if isinstance(blob, memoryview):
        blob = blob.tobytes()
    blob = blob or b""
    if blob.startswith(b"v10"):
        key = hashlib.pbkdf2_hmac("sha1", pw, b"saltysalt", 1003, 16)
        iv = b" " * 16
        proc = subprocess.run(
            ["openssl", "enc", "-d", "-aes-128-cbc", "-K", key.hex(), "-iv", iv.hex(), "-nopad"],
            input=blob[3:],
            capture_output=True,
        )
        if proc.returncode != 0 or not proc.stdout:
            fail("解开 Marvis 的 accesstoken 失败。请在系统弹窗里允许读取「Marvis Safe Storage」后重试")
        return plaintext(pkcs7(proc.stdout))
    if isinstance(plain, str) and plain:
        return plain
    if isinstance(plain, (bytes, memoryview)) and plain:
        return bytes(plain).decode("utf-8", "replace")
    if blob:
        return blob.decode("utf-8", "replace")
    return ""

vals = {}
for name in ("accesstoken", "refreshtoken", "openid", "nickname", "guid"):
    item = picked.get(name)
    if not item:
        continue
    text = decrypt(item[1], item[2]).strip()
    if name == "nickname":
        text = urllib.parse.unquote(text).strip()
        if any(ord(ch) < 32 for ch in text):
            text = ""
    vals[name] = text

access = vals.get("accesstoken", "")
if not access:
    fail("Marvis 的 Cookie 里没有 accesstoken。请先在客户端登录")

print("name: " + vals.get("nickname", ""))
print("openid: " + vals.get("openid", ""))
print("access_token: " + access)
print("refresh_token: " + vals.get("refreshtoken", ""))
print("guid: " + vals.get("guid", ""))
PY
