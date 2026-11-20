from pathlib import Path
import json
import subprocess
source = Path("configs/service/config.account.yaml")
copy = source.with_name("config.account.review.yaml")
try:
    for host, expected in [("999.1.2.3", 3), ("127.0.0.1", 0), ("2001:db8::1", 3), ("[2001:db8::1]", 0)]:
        copy.write_text(source.read_text() + '  websocket_servers:\n    "1": ws://' + host + ':7080/ws\n')
        result = subprocess.run(["../roost", "config", "check", "--service", "account", "--file", str(copy), "--json"], capture_output=True, text=True)
        data = json.loads(result.stdout)
        print(host, result.returncode, data["files"][0], flush=True)
        assert result.returncode == expected
finally:
    copy.unlink(missing_ok=True)
