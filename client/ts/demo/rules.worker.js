// Worker：规则循环在这里按经过的时间补帧（serveRules），每一步把输出发回主线程。
import { serveRules } from "../dist/index.js";
import { CounterRules } from "./rules.js";

serveRules(new CounterRules(), { stepMs: 50 });
