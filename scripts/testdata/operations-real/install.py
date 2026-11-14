#!/usr/bin/env python3
"""只改临时正式生成工程，复用真实进程夹具；不改模块或公共 harness。"""
import pathlib
import sys

repo, project = map(pathlib.Path, sys.argv[1:3])
mutant = sys.argv[3] if len(sys.argv) > 3 else ""
out = project / "e2e"
out.mkdir()
for src in (repo / "scripts/testdata/alliance-e2e").glob("*_test.go.txt"):
    (out / src.name.removesuffix(".txt")).write_text(src.read_text().replace("example.com/planet", "example.com/operations-accept"))
for src in (repo / "scripts/testdata/operations-real").glob("*_test.go.txt"):
    (out / src.name.removesuffix(".txt")).write_text(src.read_text().replace("OPERATIONS_MODULE", "example.com/operations-accept"))

controller = project / "game/controllers/purchase/controller.go"
text = controller.read_text()
anchor = "\t\tif err := c.queue.Ack(ctx, grant); err != nil {"
assert text.count(anchor) == 1, "purchase Ack fault anchor drift"
controller.write_text(text.replace(anchor, "\t\toperationsBeforeAck(grant)\n" + anchor))
src = repo / "scripts/testdata/operations-real/purchase_fault.go.txt"
(controller.parent / "operations_fault.go").write_text(src.read_text().replace("OPERATIONS_MODULE", "example.com/operations-accept"))
if mutant == "purchase-replay":
    path = project / "game/components/purchase/purchase.go"
    text = path.read_text()
    anchor = "if grant.Sequence <= applied {"
    assert text.count(anchor) == 1, "purchase replay mutation drift"
    path.write_text(text.replace(anchor, "if false { // MUTANT: replay guard removed"))
