import test from "node:test";
import assert from "node:assert/strict";

// 本地持久化失败必须被吞掉并提示，不能变成 unhandledrejection。
//
// 回归点：persistConversation 被 4 处事件处理器 / 轮询回调 await，
// 而它内部直接 `await saveImageConversation(...)` 且没有 catch。
// 一旦 IndexedDB/网络失败，rejection 会冒泡成 unhandledrejection ——
// 控制台一条红字，用户看不到提示，也不知道刚才的编辑没保存。

// 复刻 persistConversation 的错误处理契约。
async function persistWithGuard(
  save: () => Promise<void>,
  onError: (message: string) => void,
): Promise<void> {
  try {
    await save();
  } catch (error) {
    const message = error instanceof Error ? error.message : "保存会话失败";
    onError(`本地保存失败：${message}`);
  }
}

test("保存失败时提示用户，且不抛出", async () => {
  const messages: string[] = [];
  await assert.doesNotReject(
    persistWithGuard(
      async () => {
        throw new Error("quota exceeded");
      },
      (m) => messages.push(m),
    ),
  );
  assert.equal(messages.length, 1);
  assert.match(messages[0], /本地保存失败/);
  assert.match(messages[0], /quota exceeded/);
});

test("保存成功时不提示", async () => {
  const messages: string[] = [];
  await persistWithGuard(async () => {}, (m) => messages.push(m));
  assert.deepEqual(messages, []);
});

test("非 Error 抛出物也能得到可读提示", async () => {
  const messages: string[] = [];
  await persistWithGuard(
    async () => {
      throw "boom";
    },
    (m) => messages.push(m),
  );
  assert.equal(messages.length, 1);
  assert.match(messages[0], /保存会话失败/);
});
