package cleat;

import static org.junit.jupiter.api.Assertions.*;

import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

/**
 * cleat#4039. {@code cleat_fetch}, {@code cleat_json_parse} and
 * {@code cleat_json_stringify} return through the engine's {@code packSimpleResult}
 * (engine/memory.go): output length in bits 32-63, error code in bits 0-7. The Java
 * wrappers decoded them with the {@code cleat_call} decoder, whose length is bits
 * 40-63, so the length read as {@code written >> 8}: 0 for any answer under 256
 * bytes, a truncation above. Every fetch failed.
 *
 * <p>These tests hand the decode the value the engine really packs and record the
 * length it asks the reader for. A test that only pinned the two layouts would pass
 * on the old code, because both layouts were always correct -- the defect was which
 * one a wrapper used. That is why the decode lives in {@code HostCalls.simpleTextResult}
 * and {@code HostCalls.fetchResult}, which the wrappers call, and why these go through
 * them.
 */
class SimpleResultLayoutTest {

    /** What {@code packSimpleResult(errCode, extra)} returns on the host. */
    private static long packSimple(int errCode, int extra) {
        return ((long) extra << 32) | (errCode & 0xFFL);
    }

    private static final String FETCH_ANSWER =
            "{\"status\":200,\"headers\":{\"X-A\":\"b\"},\"body\":\"hello\"}";

    /** A reader that records the length it was asked for and serves the first len chars of text. */
    private static final class Reader implements java.util.function.IntFunction<String> {
        final String text;
        final AtomicInteger asked = new AtomicInteger(-1);

        Reader(String text) {
            this.text = text;
        }

        @Override
        public String apply(int len) {
            asked.set(len);
            return text.substring(0, Math.min(len, text.length()));
        }
    }

    @Test
    void aFetchAnswerUnder256BytesIsDecodedToItsRealLength() {
        assertTrue(FETCH_ANSWER.length() < 256, "the case is the short one the old decoder zeroed");
        Reader r = new Reader(FETCH_ANSWER);

        CleatResult<HostCalls.FetchResult> res = HostCalls.fetchResult(packSimple(0, FETCH_ANSWER.length()), r);

        assertEquals(FETCH_ANSWER.length(), r.asked.get(), "the length the host packed at bits 32-63");
        assertTrue(res.isOk(), "expected a decoded fetch, got: " + res.errorOrNull());
        assertEquals(200, res.getValue().statusCode);
        assertEquals("hello", res.getValue().body);
    }

    @Test
    void aFetchAnswerOver256BytesIsNotTruncated() {
        StringBuilder body = new StringBuilder();
        for (int i = 0; i < 400; i++) {
            body.append('x');
        }
        String answer = "{\"status\":200,\"headers\":{},\"body\":\"" + body + "\"}";
        Reader r = new Reader(answer);

        CleatResult<HostCalls.FetchResult> res = HostCalls.fetchResult(packSimple(0, answer.length()), r);

        assertEquals(answer.length(), r.asked.get());
        assertTrue(res.isOk(), "expected a decoded fetch, got: " + res.errorOrNull());
        assertEquals(400, res.getValue().body.length());
    }

    @Test
    void aFetchRefusalCarriesTheHostsMessageNotAnEmptyOne() {
        String msg = "fetch denied: host not allowed";
        Reader r = new Reader(msg);

        CleatResult<HostCalls.FetchResult> res = HostCalls.fetchResult(packSimple(1, msg.length()), r);

        assertTrue(res.isErr());
        assertEquals(msg, res.getError());
    }

    @Test
    void jsonParseAndStringifyShareTheLayout() {
        String canonical = "{\"a\":1}";
        Reader r = new Reader(canonical);

        CleatResult<String> res = HostCalls.simpleTextResult(packSimple(0, canonical.length()), r);

        assertEquals(canonical.length(), r.asked.get());
        assertTrue(res.isOk());
        assertEquals(canonical, res.getValue());
    }

    @Test
    void aJsonErrorCarriesTheHostsMessage() {
        String msg = "invalid JSON";
        CleatResult<String> res = HostCalls.simpleTextResult(packSimple(1, msg.length()), new Reader(msg));

        assertTrue(res.isErr());
        assertEquals(msg, res.getError());
    }

    @Test
    void aZeroLengthSuccessIsAnEmptyString() {
        CleatResult<String> res = HostCalls.simpleTextResult(packSimple(0, 0), new Reader(""));

        assertTrue(res.isOk());
        assertEquals("", res.getValue());
    }
}
