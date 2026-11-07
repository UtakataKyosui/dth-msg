// A deadline covers the whole body, rather than restarting for each chunk.
// Read outside the monitoring queue, even after the sender has authenticated.
export async function readBody(request, limit, timeoutMs = 10000) {
  const reader = request.body?.getReader();
  if (!reader) return "";
  let timer;
  try {
    const timeout = new Promise((_, reject) => {
      timer = setTimeout(() => reject(Object.assign(new Error("body read timed out"), { status: 408 })), timeoutMs);
    });
    const read = async () => {
      const bytes = new Uint8Array(limit);
      let length = 0;
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return new TextDecoder().decode(bytes.subarray(0, length));
        if (length + value.byteLength > limit) throw Object.assign(new Error("body too large"), { status: 413 });
        bytes.set(value, length);
        length += value.byteLength;
      }
    };
    return await Promise.race([read(), timeout]);
  } catch (error) {
    // Do not await a sender-controlled cancellation promise.
    void reader.cancel().catch(() => {});
    throw error;
  } finally {
    clearTimeout(timer);
  }
}
