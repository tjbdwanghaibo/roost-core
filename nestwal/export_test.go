package nestwal

import "os"

// SetSyncFileForTest 把 RR-20260926-33 的段文件 fsync 测试缝（Options.syncFile）交给同目录的外部测试包
// （nestwal_test），用来在依赖 WAL 的上层组件上注入真实的 WAL terminal。只在测试编译，生产代码不可见。
func SetSyncFileForTest(opts *Options, sync func(*os.File) error) { opts.syncFile = sync }
