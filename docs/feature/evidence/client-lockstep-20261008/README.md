# Lockstep 2026-10-08 验证日志

这些txt保留完整执行输出，只归一化CRLF、tab和行尾空白，以满足仓库diff空白检查；不删除失败、不改返回状态或测试消息。manifest.json记录规范化前后SHA256。原始日志保留在本机D:/whb_s/.tmp/client-lockstep-*.txt，原始文件名对应同轮记录。

- generated-accepted：最终正式生成build/vet、go generate托管目录字节不变、TCP/Scene race、真实C#输入/追帧与空业务注册扩展通过。
- fast-red/command-green：新增TCPSender快worker拒绝契约先失败后通过。
- race-repeat：Lockstep/wire/robot完整race三次通过；race-cancel-observed保留此前KCP关停取消失败；baseline-kcp是未改main三次定向通过，不能据此抹掉偶发观察。
- full-final：全仓153包，126通过、22无测试、5失败。baseline-*为未改main对照，解释见review验收文档。

Go build/vet/glsvet exit0（原始空输出日志client-lockstep-*-accepted.txt）；C#测试程序包括7个Go金样、坏包/补洞/容量/模拟失败、Notify顺序与取消、缺口去重/停滞重试。没有Unity实机、公网或生产资源验证结论。
