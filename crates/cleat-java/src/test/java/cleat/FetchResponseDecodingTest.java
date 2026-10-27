package cleat;

import static org.junit.jupiter.api.Assertions.*;

import org.junit.jupiter.api.Test;

/**
 * cleat#3408. {@code cleatFetch} decoded {@code status_code}, and the host answers
 * {@code status}, so every fetch reported status 0. The decoding sat inside a
 * method behind a native import, so no test could reach it; it is a static method
 * now and these pin the key handling.
 *
 * <p>The first case is the worker's real answer (cmd/cleat-worker/setup.go,
 * guardedFetch). A test whose stub says {@code status_code} would have passed
 * against the old decoder, which is how it shipped.
 */
class FetchResponseDecodingTest {

    private static HostCalls.FetchResult decode(String json) {
        CleatResult<HostCalls.FetchResult> r = HostCalls.parseFetchResponse(json);
        assertTrue(r.isOk(), "expected a decoded result, got: " + r.errorOrNull());
        return r.getValue();
    }

    @Test
    void readsTheWorkersStatusKey() {
        HostCalls.FetchResult f = decode("{\"status\":418,\"headers\":{\"X-A\":\"b\"},\"body\":\"short and stout\"}");
        assertEquals(418, f.statusCode);
        assertEquals("short and stout", f.body);
        assertEquals("b", f.headers.get("X-A"));
    }

    @Test
    void stillAcceptsTheOldKeyFromAnEmbeddersFetcher() {
        assertEquals(201, decode("{\"status_code\":201,\"body\":\"x\"}").statusCode);
    }

    @Test
    void whenBothArePresentStatusWins() {
        assertEquals(404, decode("{\"status\":404,\"status_code\":200,\"body\":\"x\"}").statusCode);
    }

    @Test
    void aPresentStatusOfZeroIsNotReplacedByTheOldKey() {
        assertEquals(0, decode("{\"status\":0,\"status_code\":200,\"body\":\"x\"}").statusCode);
    }

    @Test
    void neitherKeyIsZero() {
        assertEquals(0, decode("{\"body\":\"x\"}").statusCode);
    }

    @Test
    void aNonJsonAnswerIsAnErrorNotAZeroStatus() {
        assertTrue(HostCalls.parseFetchResponse("not json").isErr());
    }
}
