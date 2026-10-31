#!/usr/bin/env python3
"""Install application-owned Robot files before the formal protocol generator."""
from pathlib import Path
import subprocess
import sys

repo, project = map(Path, sys.argv[1:3])
genre = sys.argv[3]
module = next(
    line.split()[1]
    for line in (project / "go.mod").read_text().splitlines()
    if line.startswith("module ")
)
fixtures = repo / "scripts/testdata"
files = {
    fixtures / genre / "robot/protocol.go.txt": "protocol/def/robot.go",
    fixtures / genre / "robot/dispatch.go.txt": "game/robotaccess/dispatch.go",
    fixtures / genre / "robot/network_test.go.txt": "game/handler/robot_network_test.go",
    fixtures / "robot-common/scenario.go.txt": "loadtest/scenario.go",
    fixtures / "robot-common/main.go.txt": "cmd/robot/main.go",
}
for source, destination in files.items():
    path = project / destination
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(source.read_text().replace("GENRE_MODULE", module))

subprocess.run(
    ["gofmt", "-w", *(str(project / path) for path in files.values())],
    check=True,
)
(project / "loadtest/README.md").write_text(
    (fixtures / "robot-common/README.md").read_text()
)
