| 场景 | 视野 | 驱动 | 模式 | 代码 | P50 | P99 | 帧数 | Flush 均值 | 政策 | 政策 P95/tick | 压测线程 P50/tick | 全量 P99 | Flush 次数 | 服务端 CPU s |
| --- | ---: | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| o | 0 | direct | async | base | 31.7 / 31.8 | 50.5 / 50.2 | 91625 / 91625 | 7.3 / 7.5 | 0.0 / 0.0 | 0.0 / 0.0 | 6.1 / 6.5 | 50.3 / 50.1 | 211 / 211 | 3.5 / 3.7 |
| o | 0 | direct | async | after | 32.2 / 32.1 | 50.6 / 50.7 | 91625 / 91625 | 7.4 / 7.4 | 0.0 / 0.0 | 0.0 / 0.0 | 2.6 / 2.7 | 50.5 / 50.6 | 211 / 211 | 3.6 / 3.7 |
| o | 0 | direct | sync | base | 31.7 / 31.8 | 50.9 / 50.7 | 91625 / 91625 | 7.3 / 7.3 | 0.0 / 0.0 | 0.0 / 0.0 | 6.0 / 6.3 | 50.8 / 50.5 | 211 / 211 | 3.5 / 3.5 |
| o | 0 | direct | sync | after | 31.9 / 32.1 | 51.2 / 51.3 | 91625 / 91625 | 6.1 / 6.2 | 0.0 / 0.0 | 0.0 / 0.0 | 5.9 / 6.1 | 51.1 / 51.2 | 211 / 211 | 3.2 / 3.2 |
| o | 0 | nest | async | base | 34.2 / 34.0 | 61.4 / 61.4 | 91175 / 90625 | 9.1 / 9.1 | 2.1 / 2.0 | 3.4 / 3.5 | 0.0 / 0.0 | 55.3 / 57.1 | 213 / 213 | 5.2 / 5.2 |
| o | 0 | nest | async | after | 34.0 / 34.5 | 60.8 / 61.3 | 91144 / 91055 | 9.0 / 9.4 | 2.0 / 2.1 | 3.5 / 3.9 | 0.0 / 0.0 | 57.6 / 56.9 | 213 / 213 | 5.1 / 5.2 |
| o | 0 | nest | sync | base | 35.4 / 35.9 | 62.2 / 62.5 | 90633 / 90636 | 10.5 / 11.1 | 4.4 / 4.6 | 6.8 / 7.0 | 0.0 / 0.0 | 54.3 / 56.8 | 213 / 213 | 4.7 / 4.9 |
| o | 0 | nest | sync | after | 29.5 / 29.7 | 57.2 / 57.6 | 90539 / 90541 | 10.4 / 11.0 | 4.3 / 4.7 | 7.1 / 7.3 | 0.0 / 0.0 | 33.5 / 40.2 | 213 / 213 | 4.6 / 4.9 |
| o | 4 | direct | async | base | 32.3 / 32.1 | 49.5 / 49.7 | 190716 / 190716 | 9.4 / 9.5 | 0.0 / 0.0 | 0.0 / 0.0 | 8.8 / 8.5 | 49.4 / 49.7 | 211 / 211 | 7.2 / 7.3 |
| o | 4 | direct | async | after | 32.5 / 32.5 | 50.1 / 49.9 | 190716 / 190716 | 9.5 / 9.5 | 0.0 / 0.0 | 0.0 / 0.0 | 3.3 / 3.3 | 50.0 / 49.9 | 211 / 211 | 7.2 / 7.2 |
| o | 4 | direct | sync | base | 32.1 / 32.3 | 49.5 / 49.6 | 190716 / 190716 | 9.4 / 9.5 | 0.0 / 0.0 | 0.0 / 0.0 | 8.5 / 8.6 | 49.4 / 49.5 | 211 / 211 | 6.4 / 7.3 |
| o | 4 | direct | sync | after | 35.3 / 35.1 | 50.4 / 50.6 | 190716 / 190716 | 8.5 / 8.5 | 0.0 / 0.0 | 0.0 / 0.0 | 7.9 / 7.9 | 50.4 / 50.5 | 211 / 211 | 6.3 / 6.7 |
| o | 4 | nest | async | base | 36.4 / 36.6 | 64.1 / 63.6 | 190191 / 190103 | 12.8 / 13.2 | 3.0 / 3.1 | 4.2 / 4.4 | 0.0 / 0.0 | 61.4 / 61.8 | 213 / 213 | 9.6 / 9.7 |
| o | 4 | nest | async | after | 36.5 / 36.7 | 63.7 / 64.8 | 190150 / 190188 | 12.8 / 12.9 | 2.9 / 2.9 | 4.1 / 4.4 | 0.0 / 0.0 | 61.6 / 63.1 | 213 / 213 | 9.5 / 9.5 |
| o | 4 | nest | sync | base | 39.7 / 39.7 | 65.6 / 66.0 | 190219 / 190222 | 15.9 / 15.9 | 6.8 / 6.8 | 8.6 / 8.4 | 0.0 / 0.0 | 67.5 / 67.6 | 213 / 213 | 8.8 / 8.7 |
| o | 4 | nest | sync | after | 30.8 / 30.8 | 58.3 / 58.1 | 189961 / 190015 | 15.8 / 16.0 | 6.9 / 6.9 | 8.7 / 8.6 | 0.0 / 0.0 | 36.1 / 38.5 | 213 / 213 | 8.7 / 8.7 |
| o | 8 | direct | async | base | 34.6 / 34.5 | 49.9 / 52.3 | 200000 / 200000 | 12.9 / 13.0 | 0.0 / 0.0 | 0.0 / 0.0 | 12.7 / 12.6 | 49.7 / 52.7 | 211 / 211 | 11.3 / 11.3 |
| o | 8 | direct | async | after | 35.1 / 35.3 | 53.3 / 52.7 | 200000 / 200000 | 12.8 / 12.9 | 0.0 / 0.0 | 0.0 / 0.0 | 5.0 / 5.0 | 53.2 / 52.9 | 211 / 211 | 11.4 / 11.5 |
| o | 8 | direct | sync | base | 34.6 / 34.6 | 50.1 / 51.1 | 200000 / 200000 | 12.8 / 13.1 | 0.0 / 0.0 | 0.0 / 0.0 | 12.6 / 12.7 | 49.9 / 50.9 | 211 / 211 | 11.2 / 11.5 |
| o | 8 | direct | sync | after | 41.4 / 41.4 | 60.2 / 59.1 | 200000 / 200000 | 12.5 / 12.1 | 0.0 / 0.0 | 0.0 / 0.0 | 12.0 / 12.1 | 60.6 / 58.2 | 211 / 211 | 11.2 / 11.1 |
| o | 8 | nest | async | base | 41.5 / 40.3 | 71.2 / 73.0 | 199938 / 199997 | 19.0 / 18.2 | 4.7 / 4.5 | 6.1 / 6.1 | 0.0 / 0.0 | 70.5 / 65.7 | 213 / 213 | 14.9 / 14.3 |
| o | 8 | nest | async | after | 41.7 / 41.2 | 70.3 / 70.1 | 199947 / 199970 | 18.7 / 18.8 | 4.7 / 4.7 | 6.0 / 6.1 | 0.0 / 0.0 | 68.5 / 63.0 | 213 / 213 | 14.8 / 14.8 |
| o | 8 | nest | sync | base | 46.4 / 47.1 | 78.2 / 74.9 | 200000 / 199999 | 23.6 / 23.7 | 10.2 / 10.3 | 12.5 / 12.5 | 0.0 / 0.0 | 75.2 / 73.8 | 213 / 213 | 13.9 / 13.7 |
| o | 8 | nest | sync | after | 35.9 / 35.8 | 70.6 / 73.5 | 199642 / 199705 | 23.8 / 23.8 | 10.6 / 10.8 | 12.8 / 12.8 | 0.0 / 0.0 | 43.2 / 41.9 | 213 / 213 | 13.9 / 13.8 |
| o | 16 | direct | async | base | 36.6 / 36.7 | 60.9 / 61.0 | 200000 / 200000 | 20.0 / 19.5 | 0.0 / 0.0 | 0.0 / 0.0 | 20.6 / 20.5 | 61.5 / 62.5 | 211 / 211 | 18.1 / 18.1 |
| o | 16 | direct | async | after | 40.7 / 40.7 | 70.0 / 74.6 | 200000 / 200000 | 19.6 / 20.9 | 0.0 / 0.0 | 0.0 / 0.0 | 8.6 / 9.1 | 68.7 / 73.0 | 211 / 211 | 18.1 / 18.3 |
| o | 16 | direct | sync | base | 36.7 / 36.5 | 64.0 / 63.1 | 200000 / 200000 | 19.6 / 19.6 | 0.0 / 0.0 | 0.0 / 0.0 | 20.4 / 20.4 | 63.2 / 62.2 | 211 / 211 | 18.2 / 18.2 |
| o | 16 | direct | sync | after | 43.5 / 43.5 | 82.6 / 77.2 | 200000 / 200000 | 19.5 / 20.2 | 0.0 / 0.0 | 0.0 / 0.0 | 20.3 / 20.3 | 82.6 / 76.1 | 211 / 211 | 17.8 / 18.2 |
| o | 16 | nest | async | base | 50.9 / 51.4 | 128.5 / 111.9 | 197999 / 199000 | 31.3 / 31.4 | 9.4 / 9.0 | 21.5 / 15.4 | 0.0 / 0.0 | 74.8 / 77.9 | 211 / 213 | 23.8 / 24.2 |
| o | 16 | nest | async | after | 51.3 / 51.0 | 116.6 / 110.5 | 198000 / 198000 | 31.5 / 30.9 | 9.2 / 8.9 | 15.1 / 17.6 | 0.0 / 0.0 | 77.2 / 69.6 | 211 / 211 | 24.3 / 23.8 |
| o | 16 | nest | sync | base | 59.3 / 59.6 | 122.2 / 130.6 | 198000 / 196000 | 39.4 / 39.9 | 18.2 / 18.3 | 31.1 / 28.2 | 0.0 / 0.0 | 87.4 / 87.8 | 211 / 209 | 22.8 / 22.1 |
| o | 16 | nest | sync | after | 47.9 / 46.7 | 132.5 / 123.6 | 193000 / 196982 | 42.2 / 40.6 | 19.8 / 19.4 | 38.3 / 38.0 | 0.0 / 0.0 | 46.4 / 49.8 | 206 / 210 | 23.6 / 22.6 |

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
- o-v4-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v4-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- o-v8-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
