#!/bin/bash
# Prepare private runtime data without printing keys or touching account tokens.
set +x
set -euo pipefail

if ! command -v python3 >/dev/null 2>&1; then
  echo "prepare 需要 python3（仅使用标准库）。" >&2
  exit 1
fi

ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
python3 - "$ROOT" "$@" <<'PY'
import argparse
import json
import os
from pathlib import Path
import re
import stat
import sys
import tempfile

root = Path(sys.argv[1])
parser = argparse.ArgumentParser(description="为 Docker 准备签名钥匙和已注册设备号；不导出账号 token。")
parser.add_argument("--import", dest="import_file", type=Path, help="导入已有的 prepare JSON（适用于 Linux）")
parser.add_argument("--client-dir", type=Path, help="客户端数据目录，包含 OfflinePack 和 device-info.cache")
parser.add_argument("--app-dir", type=Path, default=Path("/Applications/Marvis.app"), help="官方 App 目录，作为离线包备用来源")
parser.add_argument("--output", type=Path, default=root / ".prepare/marvis.json", help="输出文件；父目录必须为当前用户私有")
args = parser.parse_args(sys.argv[2:])

MAX_FILE_SIZE = 64 * 1024
KEY_PATTERN = re.compile(r'''(?<![\w$])(?:marvis_client|["']marvis_client["'])\s*:\s*(["'])([^"'\s\\]{1,256})\1''')
QIMEI_PATTERN = re.compile(r"(?<![0-9a-fA-F])[0-9a-fA-F]{36}(?![0-9a-fA-F])")


def validate(data):
    if not isinstance(data, dict):
        raise ValueError("prepare 文件必须是 JSON 对象")
    key, qimei = data.get("access_key"), data.get("qimei36")
    if not isinstance(key, str) or not key.strip() or len(key.strip()) > 256 or any(ord(c) < 33 or ord(c) > 126 for c in key.strip()):
        raise ValueError("缺少有效的 access_key")
    if not isinstance(qimei, str) or not re.fullmatch(r"[0-9a-fA-F]{36}", qimei.strip()):
        raise ValueError("缺少已注册的 36 位十六进制 qimei36；不能使用随机生成的 UUID")
    return {"access_key": key.strip(), "qimei36": qimei.strip().lower()}


def import_data(path):
    with path.expanduser().open("rb") as f:
        raw = f.read(MAX_FILE_SIZE + 1)
    if len(raw) > MAX_FILE_SIZE:
        raise ValueError("prepare 文件不能超过 64 KiB")
    try:
        data = json.loads(raw)
    except (ValueError, UnicodeError):
        raise ValueError("prepare 文件不是有效的 UTF-8 JSON") from None
    return validate(data)


def extract_data():
    client_dir = args.client_dir
    if client_dir is None:
        if sys.platform != "darwin":
            raise ValueError("Linux 请用 --import 导入已有 prepare 文件，或用 --client-dir 指定已有客户端数据；不能自动注册设备号")
        client_dir = Path.home() / "Library/Application Support/com.tencent.mac.marvis"
    client_dir = client_dir.expanduser()
    sources = [client_dir / "OfflinePack/main/current/assets", args.app_dir.expanduser() / "Contents/Resources/offline-pack/main/assets"]
    access_key = None
    for directory in sources:
        keys = set()
        # Hashed filenames change between releases, so inspect the SDK property.
        for path in sorted(directory.glob("*.js")):
            if path.stat().st_size > 32 * 1024 * 1024:
                continue
            text = path.read_text(encoding="utf-8", errors="replace")
            keys.update(match[2] for match in KEY_PATTERN.finditer(text))
        if len(keys) > 1:
            raise ValueError("离线包里有多个不同的 marvis_client 钥匙，拒绝猜测；请确认客户端版本或导入已核对的文件")
        if keys:
            access_key = keys.pop()
            break
    if access_key is None:
        raise ValueError("未在客户端离线包中找到 marvis_client 签名钥匙；请先安装并运行官方客户端，或指定 --client-dir / --app-dir")
    device_file = client_dir / "device-info.cache"
    if not device_file.is_file():
        raise ValueError("没有 device-info.cache；请先运行官方客户端完成设备注册，或导入已有 prepare 文件")
    with device_file.open("rb") as f:
        raw = f.read(MAX_FILE_SIZE + 1)
    if len(raw) > MAX_FILE_SIZE:
        raise ValueError("device-info.cache 超过 64 KiB")
    qimeis = {m.lower() for m in QIMEI_PATTERN.findall(raw.decode("utf-8", errors="replace"))}
    if len(qimeis) != 1:
        raise ValueError("device-info.cache 未包含唯一的 36 位设备号，拒绝猜测")
    return validate({"access_key": access_key, "qimei36": qimeis.pop()})


def save(data, path):
    path = path.expanduser().absolute()
    parent = path.parent
    parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    info = parent.stat()
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
        raise ValueError("输出目录必须归当前用户所有且权限为 0700；请使用私有目录")
    fd, tmp = tempfile.mkstemp(prefix=".marvis-", dir=parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
            # The 0700 host directory protects the file. 0644 lets the non-root
            # container UID read a single-file, read-only bind mount on Linux.
            os.fchmod(f.fileno(), 0o644)
        os.replace(tmp, path)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)
    print("prepare 已生成：" + str(path))
    print("只包含签名钥匙和设备号，不包含账号 token；请勿提交或复制进镜像。")


try:
    if args.import_file and args.client_dir:
        raise ValueError("--import 与 --client-dir 不能同时使用")
    os.umask(0o077)
    data = import_data(args.import_file) if args.import_file else extract_data()
    save(data, args.output)
except (OSError, ValueError) as exc:
    print("prepare 失败：" + str(exc), file=sys.stderr)
    sys.exit(1)
PY
