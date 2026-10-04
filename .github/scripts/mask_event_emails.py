"""Mask event emails in Actions logs and remove them from action metadata."""
import json
import os
from pathlib import Path


def redact_emails(value, register_mask):
    if isinstance(value, dict):
        for key, item in value.items():
            if key.lower() == "email" and isinstance(item, str) and item:
                register_mask(item)
                value[key] = "[redacted]"
            else:
                redact_emails(item, register_mask)
    elif isinstance(value, list):
        for item in value:
            redact_emails(item, register_mask)


def add_mask(value):
    escaped = value.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    print("::add-mask::" + escaped)


def main():
    path = Path(os.environ["GITHUB_EVENT_PATH"])
    payload = json.loads(path.read_text(encoding="utf-8"))
    redact_emails(payload, add_mask)
    path.write_text(json.dumps(payload), encoding="utf-8")


if __name__ == "__main__":
    main()
