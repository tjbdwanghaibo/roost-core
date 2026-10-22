| 场景 | 视野 | 驱动 | 模式 | 代码 | P50 | P99 | 帧数 | Flush 均值 | 政策 | 政策 P95/tick | 压测线程 P50/tick | 全量 P99 | Flush 次数 | 服务端 CPU s |
| --- | ---: | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| o | 0 | direct | async | base | 31.7 / 31.5 | 50.4 / 50.5 | 91625 / 91625 | 7.3 / 7.1 | 0.0 / 0.0 | 0.0 / 0.0 | 6.2 / 6.2 | 50.2 / 50.4 | 211 / 211 | 3.6 / 3.5 |
| o | 0 | direct | async | after | 32.1 | 50.7 | 91625 | 7.4 | 0.0 | 0.0 | 2.7 | 50.7 | 211 | 3.7 |
| o | 0 | direct | sync | base | 31.9 / 31.7 | 50.7 / 50.6 | 91625 / 91625 | 7.6 / 7.3 | 0.0 / 0.0 | 0.0 / 0.0 | 6.2 / 6.2 | 50.4 / 50.4 | 211 / 211 | 3.7 / 3.5 |
| o | 0 | direct | sync | after | 32.1 / 32.1 | 51.2 / 51.0 | 91625 / 91625 | 6.2 / 6.2 | 0.0 / 0.0 | 0.0 / 0.0 | 6.2 / 6.0 | 51.2 / 51.0 | 211 / 211 | 3.3 / 3.2 |
| o | 0 | nest | async | base | 32.0 / 32.2 | 58.1 / 59.1 | 91193 / 91095 | 7.2 / 6.9 | 0.3 / 0.3 | 1.3 / 1.1 | 1.6 / 1.7 | 55.1 / 56.8 | 213 / 213 | 5.1 / 4.8 |
| o | 0 | nest | async | after | 32.0 / 32.0 | 57.6 / 57.6 | 90649 / 90636 | 7.3 / 7.4 | 0.0 / 0.0 | 0.0 / 0.0 | 2.8 / 2.8 | 54.6 / 56.2 | 213 / 213 | 5.4 / 5.4 |
| o | 0 | nest | sync | base | 35.8 / 36.3 | 62.4 / 62.7 | 90613 / 90623 | 11.0 / 11.3 | 4.6 / 4.8 | 6.7 / 7.1 | 0.0 / 0.0 | 61.0 / 60.2 | 213 / 213 | 4.8 / 4.9 |
| o | 0 | nest | sync | after | 31.0 / 30.9 | 56.6 / 57.0 | 90657 / 90614 | 7.0 / 7.1 | 0.8 / 0.9 | 2.6 / 2.6 | 5.0 / 5.0 | 55.0 / 51.7 | 213 / 213 | 4.8 / 4.7 |
| o | 4 | direct | async | base | 32.2 | 49.6 | 190716 | 9.4 | 0.0 | 0.0 | 8.5 | 49.5 | 211 | 7.2 |
| o | 4 | direct | async | after | 32.6 | 50.1 | 190716 | 9.5 | 0.0 | 0.0 | 3.3 | 50.1 | 211 | 7.2 |
| o | 4 | direct | sync | base | 32.4 | 49.5 | 190716 | 9.5 | 0.0 | 0.0 | 8.5 | 49.5 | 211 | 6.5 |
| o | 4 | direct | sync | after | 35.1 | 50.6 | 190716 | 8.4 | 0.0 | 0.0 | 7.9 | 50.5 | 211 | 6.2 |
| o | 4 | nest | async | base | 33.7 | 61.2 | 190125 | 10.7 | 0.6 | 2.0 | 2.7 | 58.2 | 213 | 9.5 |
| o | 4 | nest | async | after | 33.1 | 59.1 | 190286 | 10.1 | 0.0 | 0.0 | 3.4 | 60.9 | 213 | 9.6 |
| o | 4 | nest | sync | base | 39.5 | 65.5 | 190201 | 15.8 | 6.7 | 8.4 | 0.0 | 67.2 | 213 | 8.8 |
| o | 4 | nest | sync | after | 33.2 | 60.0 | 189943 | 11.4 | 2.5 | 5.1 | 6.7 | 56.8 | 213 | 8.6 |
| o | 8 | direct | async | base | 34.5 / 34.7 | 51.2 / 50.5 | 200000 / 200000 | 13.1 / 13.0 | 0.0 / 0.0 | 0.0 / 0.0 | 12.6 / 12.7 | 52.9 / 50.2 | 211 / 211 | 11.6 / 11.4 |
| o | 8 | direct | async | after | 35.4 / 35.1 | 55.4 / 55.1 | 200000 / 200000 | 12.9 / 12.8 | 0.0 / 0.0 | 0.0 / 0.0 | 5.1 / 5.0 | 56.3 / 55.0 | 211 / 211 | 11.5 / 11.3 |
| o | 8 | direct | sync | base | 34.6 | 55.2 | 200000 | 12.9 | 0.0 | 0.0 | 12.6 | 55.3 | 211 | 11.4 |
| o | 8 | direct | sync | after | 41.5 | 61.4 | 200000 | 12.3 | 0.0 | 0.0 | 12.1 | 61.2 | 211 | 11.1 |
| o | 8 | nest | async | base | 36.1 / 36.5 | 70.8 / 72.5 | 199858 / 199634 | 15.6 / 15.9 | 1.6 / 1.8 | 4.1 / 4.1 | 4.1 / 3.9 | 71.9 / 66.5 | 213 / 213 | 14.9 / 14.8 |
| o | 8 | nest | async | after | 36.8 / 36.8 | 64.7 / 66.6 | 199999 / 199696 | 15.1 / 14.7 | 0.8 / 0.8 | 2.4 / 2.5 | 5.1 / 5.0 | 71.8 / 70.0 | 213 / 213 | 14.8 / 14.6 |
| o | 8 | nest | sync | base | 46.9 | 75.3 | 200000 | 23.8 | 10.4 | 12.7 | 0.0 | 73.3 | 213 | 13.9 |
| o | 8 | nest | sync | after | 39.9 | 76.9 | 199870 | 18.1 | 4.9 | 6.9 | 9.9 | 64.0 | 213 | 13.9 |
| o | 16 | direct | async | base | 37.0 / 36.8 | 56.2 / 60.2 | 200000 / 200000 | 20.2 / 19.6 | 0.0 / 0.0 | 0.0 / 0.0 | 20.5 / 20.7 | 55.2 / 60.0 | 211 / 211 | 18.2 / 17.7 |
| o | 16 | direct | async | after | 40.5 / 40.8 | 71.8 / 72.8 | 200000 / 200000 | 19.4 / 19.6 | 0.0 / 0.0 | 0.0 / 0.0 | 9.0 / 9.0 | 71.8 / 72.2 | 211 / 211 | 17.8 / 17.6 |
| o | 16 | direct | sync | base | 36.8 / 36.7 | 59.1 / 59.5 | 200000 / 200000 | 19.8 / 19.7 | 0.0 / 0.0 | 0.0 / 0.0 | 20.7 / 20.5 | 59.1 / 59.5 | 211 / 211 | 17.8 / 17.9 |
| o | 16 | direct | sync | after | 43.5 / 43.6 | 81.9 / 82.3 | 200000 / 200000 | 20.2 / 19.5 | 0.0 / 0.0 | 0.0 / 0.0 | 20.5 / 20.3 | 82.1 / 82.2 | 211 / 211 | 18.2 / 17.9 |
| o | 16 | nest | async | base | 45.7 / 45.1 | 92.9 / 97.9 | 200000 / 200000 | 27.5 / 27.9 | 6.0 / 6.3 | 13.0 / 16.1 | 7.1 / 6.6 | 92.9 / 59.4 | 213 / 213 | 24.1 / 24.1 |
| o | 16 | nest | async | after | 46.4 / 46.3 | 126.6 / 107.6 | 197000 / 197913 | 27.2 / 27.1 | 5.4 / 5.1 | 11.0 / 11.8 | 8.9 / 9.1 | 85.6 / 96.2 | 210 / 211 | 23.7 / 24.1 |
| o | 16 | nest | sync | base | 59.3 / 59.5 | 122.2 / 122.7 | 197000 / 197000 | 39.5 / 39.5 | 18.2 / 18.3 | 27.2 / 28.0 | 0.0 / 0.0 | 87.3 / 88.3 | 210 / 210 | 22.6 / 22.2 |
| o | 16 | nest | sync | after | 54.0 / 53.6 | 153.8 / 148.4 | 195000 / 194995 | 30.9 / 30.8 | 9.2 / 9.4 | 15.7 / 16.0 | 16.8 / 16.7 | 112.1 / 107.4 | 208 / 208 | 23.3 / 22.4 |

异常运行：
- o-v0-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v0-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 32504: context canceled', 'client_error': None}
- o-v16-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v16-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
