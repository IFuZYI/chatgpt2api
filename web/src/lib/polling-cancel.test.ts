import test from "node:test";
import assert from "node:assert/strict";

// 轮询循环必须在组件卸载后停止。
//
// 回归点：runConversationQueue 里是 `while (true) { ... await sleep(2000) ... }`，
// 唯一退出条件是「没有 loading 任务」或「连续 10 次错误」。用户切走页面后
// 循环继续每 2 秒请求 /api/image-tasks，直到任务自然结束 —— 而 React 18
// 起 setState-after-unmount 不再告警，所以这个泄漏完全静默。

// 复刻循环的取消契约：unmounted 为 true 时立刻返回，不再调用 fetch。
async function runPollLoop(opts: {
  unmounted: () => boolean;
  hasLoading: () => boolean;
  fetch: () => Promise<void>;
  sleep: () => Promise<void>;
}): Promise<number> {
  let calls = 0;
  while (true) {
    if (opts.unmounted()) {
      return calls;
    }
    if (!opts.hasLoading()) {
      return calls;
    }
    await opts.sleep();
    await opts.fetch();
    calls += 1;
    if (calls > 100) {
      throw new Error("循环没有退出");
    }
  }
}

test("卸载后立即停止，不再发请求", async () => {
  let unmounted = false;
  let fetches = 0;
  const loop = runPollLoop({
    unmounted: () => unmounted,
    hasLoading: () => true,
    fetch: async () => {
      fetches += 1;
      unmounted = true; // 第一次请求后用户切走页面
    },
    sleep: async () => {},
  });
  await loop;
  assert.equal(fetches, 1, "卸载后不应继续请求");
});

test("任务完成后正常退出", async () => {
  let loading = true;
  let fetches = 0;
  await runPollLoop({
    unmounted: () => false,
    hasLoading: () => loading,
    fetch: async () => {
      fetches += 1;
      if (fetches === 3) loading = false;
    },
    sleep: async () => {},
  });
  assert.equal(fetches, 3);
});
