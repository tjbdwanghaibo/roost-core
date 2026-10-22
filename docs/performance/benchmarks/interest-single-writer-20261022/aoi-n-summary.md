| 场景 | 视野 | 驱动 | 模式 | 代码 | P50 | P99 | 帧数 | Flush 均值 | 政策 | 政策 P95/tick | 压测线程 P50/tick | 全量 P99 | Flush 次数 | 服务端 CPU s |
| --- | ---: | --- | --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| n | 4 | direct | async | base | 32.8 / 32.6 | 50.4 / 50.8 | 80240 / 80240 | 8.0 / 8.1 | 0.0 / 0.0 | 0.0 / 0.0 | 3.8 / 3.8 | 50.6 / 50.6 | 211 / 211 | 3.6 / 3.6 |
| n | 4 | direct | async | after | 32.8 / 32.9 | 51.1 / 51.2 | 80240 / 80240 | 8.1 / 8.1 | 0.0 / 0.0 | 0.0 / 0.0 | 1.3 / 1.3 | 51.6 / 50.9 | 211 / 211 | 3.8 / 3.7 |
| n | 4 | direct | sync | base | 32.8 / 32.9 | 50.4 / 50.7 | 80240 / 80240 | 8.1 / 8.1 | 0.0 / 0.0 | 0.0 / 0.0 | 3.6 / 3.8 | 50.6 / 50.8 | 211 / 211 | 3.6 / 3.6 |
| n | 4 | direct | sync | after | 32.1 / 31.9 | 51.4 / 50.8 | 80240 / 80240 | 6.8 / 6.7 | 0.0 / 0.0 | 0.0 / 0.0 | 4.7 / 4.6 | 51.4 / 51.1 | 211 / 211 | 3.4 / 3.3 |
| n | 4 | nest | async | base | 33.6 / 34.2 | 60.7 / 61.1 | 80149 / 80510 | 8.6 / 8.6 | 0.8 / 0.8 | 1.8 / 1.6 | 0.0 / 0.0 | 56.3 / 55.5 | 213 / 213 | 6.5 / 6.7 |
| n | 4 | nest | async | after | 34.1 / 34.2 | 60.6 / 61.3 | 80165 / 80181 | 8.8 / 8.7 | 0.8 / 0.8 | 1.6 / 1.6 | 0.0 / 0.0 | 58.2 / 52.5 | 213 / 213 | 6.6 / 6.7 |
| n | 4 | nest | sync | base | 35.3 / 35.4 | 61.1 / 61.3 | 80216 / 80205 | 10.4 / 10.3 | 3.4 / 3.4 | 5.8 / 5.5 | 0.0 / 0.0 | 58.3 / 54.1 | 213 / 213 | 5.8 / 5.9 |
| n | 4 | nest | sync | after | 29.4 / 29.6 | 56.4 / 56.4 | 79905 / 79940 | 10.0 / 10.2 | 3.3 / 3.4 | 5.8 / 6.2 | 0.0 / 0.0 | 36.1 / 35.0 | 213 / 213 | 5.7 / 5.8 |
| n | 8 | direct | async | base | 33.6 / 33.7 | 50.2 / 50.4 | 119597 / 119597 | 10.0 / 10.1 | 0.0 / 0.0 | 0.0 / 0.0 | 5.8 / 5.9 | 50.4 / 50.3 | 211 / 211 | 4.4 / 4.4 |
| n | 8 | direct | async | after | 33.9 / 33.9 | 50.9 / 51.1 | 119597 / 119597 | 10.0 / 9.9 | 0.0 / 0.0 | 0.0 / 0.0 | 1.9 / 1.8 | 50.7 / 50.8 | 211 / 211 | 4.5 / 4.4 |
| n | 8 | direct | sync | base | 33.2 / 33.6 | 50.4 / 50.4 | 119597 / 119597 | 9.6 / 10.1 | 0.0 / 0.0 | 0.0 / 0.0 | 5.3 / 5.6 | 50.3 / 50.4 | 211 / 211 | 4.1 / 4.3 |
| n | 8 | direct | sync | after | 32.4 / 32.6 | 51.4 / 51.3 | 119597 / 119597 | 7.2 / 7.3 | 0.0 / 0.0 | 0.0 / 0.0 | 5.5 / 5.8 | 51.8 / 51.1 | 211 / 211 | 3.7 / 3.8 |
| n | 8 | nest | async | base | 34.5 / 34.3 | 61.4 / 61.0 | 119752 / 119519 | 10.6 / 10.6 | 1.0 / 1.0 | 2.0 / 2.0 | 0.0 / 0.0 | 52.8 / 52.5 | 213 / 213 | 6.7 / 6.8 |
| n | 8 | nest | async | after | 34.6 / 34.5 | 61.7 / 61.4 | 119509 / 119478 | 10.5 / 10.5 | 1.0 / 1.0 | 1.9 / 1.8 | 0.0 / 0.0 | 52.5 / 53.0 | 213 / 213 | 6.8 / 6.6 |
| n | 8 | nest | sync | base | 35.4 / 35.8 | 61.9 / 61.9 | 119576 / 119530 | 11.1 / 11.2 | 3.7 / 3.6 | 5.9 / 6.1 | 0.0 / 0.0 | 52.4 / 53.5 | 213 / 213 | 5.5 / 5.6 |
| n | 8 | nest | sync | after | 29.8 / 30.2 | 57.3 / 57.3 | 119555 / 119612 | 11.4 / 11.5 | 4.0 / 3.9 | 6.4 / 6.5 | 0.0 / 0.0 | 32.6 / 32.9 | 213 / 213 | 5.7 / 5.7 |

异常运行：
- n-v4-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v4-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 104184: context canceled', 'client_error': None}
- n-v8-async-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-async-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-async-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-async-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-sync-nest-after-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-sync-nest-after-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-sync-nest-base-r1 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
- n-v8-sync-nest-base-r2 {'error': None, 'flush_failures': 1, 'manager_last_error': 'sync admission session 1784: context canceled', 'client_error': None}
