using Cleat.Runtime;
using System.Text.Json;
using Xunit;

namespace Cleat.Tests;

public class ExceptionMappingTests
{
    // The mapping is python-sdk/cleat_sdk/host_calls.py's _CALL_ERROR_CODE_MAP.
    [Theory]
    [InlineData(0u, typeof(CleatCallException))]
    [InlineData(1u, typeof(CleatCallTimeoutException))]
    [InlineData(2u, typeof(CleatCallTransientException))]
    [InlineData(3u, typeof(CleatCallPermanentException))]
    [InlineData(4u, typeof(CleatCallPermanentException))]
    [InlineData(5u, typeof(CleatCallPermanentException))]
    [InlineData(99u, typeof(CleatCallException))]
    public void CodeSelectsClass(uint code, Type expected)
    {
        var e = CleatCallException.For("svc", "op", "msg", code);
        Assert.Equal(expected, e.GetType());
        Assert.Equal(code, e.Code);
        Assert.Equal("msg", e.Message);
    }

    [Fact]
    public void TimeoutIsTransient() => Assert.IsAssignableFrom<CleatCallTransientException>(CleatCallException.For("s", "o", "m", 1));

    [Fact]
    public void UnknownCodeIsNotClassified()
    {
        var e = CleatCallException.For("s", "o", "m", 99);
        Assert.False(e is CleatCallTransientException || e is CleatCallPermanentException);
    }
}

public class PackedTests
{
    [Fact] public void SentinelSuspends() => Assert.True(Packed.SleepMustSuspend(1UL << 31));
    [Fact] public void StatusByteSuspends() => Assert.True(Packed.SleepMustSuspend((1UL << 56) | 400));
    [Fact] public void CompletedDoesNotSuspend() => Assert.False(Packed.SleepMustSuspend(0));
    [Fact] public void CompletedWithDurationDoesNotSuspend() => Assert.False(Packed.SleepMustSuspend(400));

    // The sentinel is tested by whole-word equality: a word that merely has bit 31 set along with other
    // fields is a payload, not a stop.
    [Fact] public void BitThirtyOnePlusFieldsIsNotTheSentinel() => Assert.False(Packed.SleepMustSuspend((1UL << 31) | 5));

    // Other statuses (anything but 1) do not suspend.
    [Fact] public void OtherStatusDoesNotSuspend() => Assert.False(Packed.SleepMustSuspend(2UL << 56));
}

public class DeferTableTests
{
    private static DeferTable Table(List<string> phases) => new(on => phases.Add(on ? "on" : "off"));

    [Fact]
    public void RunsLastRegisteredFirst()
    {
        var order = new List<string>();
        var t = Table(new());
        t.Register("a", () => { order.Add("a"); return Task.CompletedTask; });
        t.Register("b", () => { order.Add("b"); return Task.CompletedTask; });
        t.Register("c", () => { order.Add("c"); return Task.CompletedTask; });
        Assert.Equal(3u, t.Drain(true));
        Assert.Equal(new[] { "c", "b", "a" }, order);
    }

    [Fact]
    public void SecondDrainRunsNothing()
    {
        var ran = 0;
        var t = Table(new());
        t.Register("a", () => { ran++; return Task.CompletedTask; });
        t.Drain(true);
        Assert.Equal(0u, t.Drain(true));
        Assert.Equal(1, ran);
    }

    [Fact]
    public void TableIsEmptiedBeforeTheFirstBodyRuns()
    {
        var t = Table(new());
        var seen = -1;
        t.Register("a", () => { seen = t.Count; return Task.CompletedTask; });
        t.Drain(true);
        Assert.Equal(0, seen);
    }

    [Fact]
    public void OneFailingBodyDoesNotStopTheOthers()
    {
        var ran = new List<string>();
        var t = Table(new());
        t.Register("a", () => { ran.Add("a"); return Task.CompletedTask; });
        t.Register("b", () => throw new InvalidOperationException("boom"));
        t.Register("c", () => { ran.Add("c"); return Task.CompletedTask; });
        Assert.Equal(3u, t.Drain(true));
        Assert.Equal(new[] { "c", "a" }, ran);
    }

    [Fact]
    public void GuestDrainPropagatesASuspendingBody()
    {
        var t = Table(new());
        t.Register("a", () => throw new CleatSuspendException());
        Assert.Throws<CleatSuspendException>(() => t.Drain(propagateSuspend: true));
    }

    [Fact]
    public void HostDrainSwallowsASuspendAndRunsTheRest()
    {
        var ran = new List<string>();
        var t = Table(new());
        t.Register("a", () => { ran.Add("a"); return Task.CompletedTask; });
        t.Register("b", () => throw new CleatSuspendException());
        Assert.Equal(2u, t.Drain(propagateSuspend: false));
        Assert.Equal(new[] { "a" }, ran);
    }

    [Fact]
    public void PhaseIsBracketedAroundTheDrain()
    {
        var phases = new List<string>();
        var t = Table(phases);
        t.Register("a", () => { phases.Add("body"); return Task.CompletedTask; });
        t.Drain(true);
        Assert.Equal(new[] { "on", "body", "off" }, phases);
    }

    [Fact]
    public void PhaseIsClearedWhenASuspendPropagates()
    {
        var phases = new List<string>();
        var t = Table(phases);
        t.Register("a", () => throw new CleatSuspendException());
        Assert.Throws<CleatSuspendException>(() => t.Drain(true));
        Assert.Equal(new[] { "on", "off" }, phases);
    }

    [Fact]
    public void EmptyTableMakesNoPhaseCalls()
    {
        var phases = new List<string>();
        Assert.Equal(0u, Table(phases).Drain(true));
        Assert.Empty(phases);
    }

    [Fact]
    public void AsyncBodiesAreAwaited()
    {
        var done = false;
        var t = Table(new());
        t.Register("a", async () => { await Task.CompletedTask; done = true; });
        t.Drain(true);
        Assert.True(done);
    }

    // ---- reserve first (cleat#4079): durable-defer can stop the guest AFTER the host recorded the registration ----

    private static Func<Task> Body(List<string> ran, string name) => () => { ran.Add(name); return Task.CompletedTask; };

    [Fact]
    public void TheBodyIsOnTheTableBeforeTheHostIsAsked()
    {
        var t = Table(new());
        IReadOnlyList<string>? seen = null;
        t.Register(() => { seen = t.Ids(); return "d1"; }, Body(new(), "a"));
        // One entry, not yet carrying an id, at the moment the host call runs. Appending after the call would read [].
        Assert.Equal(new[] { "" }, seen);
        Assert.Equal(new[] { "d1" }, t.Ids());
    }

    [Fact]
    public void TheHostsIdReplacesTheEntryInPlaceAndTheOrderIsUnchanged()
    {
        var ran = new List<string>();
        var t = Table(new());
        t.Register(() => "A", Body(ran, "a"));
        t.Register(() => "B", Body(ran, "b"));
        t.Register(() => "C", Body(ran, "c"));
        Assert.Equal(new[] { "A", "B", "C" }, t.Ids());
        t.Drain(true);
        Assert.Equal(new[] { "c", "b", "a" }, ran);
    }

    [Fact]
    public void TheDrainDoesNotLookABodyUpByItsId()
    {
        // Two registrations that came back with the SAME id: a lookup by id could run only one body. The id is a label.
        var ran = new List<string>();
        var t = Table(new());
        t.Register(() => "same", Body(ran, "a"));
        t.Register(() => "same", Body(ran, "b"));
        Assert.Equal(2u, t.Drain(true));
        Assert.Equal(new[] { "b", "a" }, ran);
    }

    [Fact]
    public void AStopKeepsTheEntryPropagatesAndTheDrainRunsIt()
    {
        var ran = new List<string>();
        var t = Table(new());
        t.Register(() => "A", Body(ran, "a"));
        var calls = 0;
        Assert.Throws<CleatSuspendException>(() =>
            t.Register(() => { calls++; throw new CleatSuspendException(); }, Body(ran, "b")));
        // The stop arrived after the host recorded "b", so "b" must still be there to run, in its place.
        Assert.Equal(1, calls);
        Assert.Equal(new[] { "A", "" }, t.Ids());
        Assert.Equal(2u, t.Drain(true));
        Assert.Equal(new[] { "b", "a" }, ran);
    }

    [Fact]
    public void AnyOtherFailureDiscardsTheEntryAndSurfacesTheFailure()
    {
        var ran = new List<string>();
        var t = Table(new());
        t.Register(() => "A", Body(ran, "a"));
        var boom = Assert.Throws<CleatCallPermanentException>(() =>
            t.Register(() => throw new CleatCallPermanentException("workflow", "defer", "refused", 3u), Body(ran, "b")));
        Assert.Contains("refused", boom.Message);
        // The host never registered "b", so it must not run, and "a" is untouched.
        Assert.Equal(new[] { "A" }, t.Ids());
        Assert.Equal(1u, t.Drain(true));
        Assert.Equal(new[] { "a" }, ran);
    }

    [Fact]
    public void AStopFromTheFirstRegistrationStillLeavesThatBodyToRun()
    {
        // The case the whole change exists for: a killed run stops at its FIRST durable-defer. Nothing is on the table
        // but the body that call was registering.
        var ran = new List<string>();
        var t = Table(new());
        Assert.Throws<CleatSuspendException>(() =>
            t.Register(() => throw new CleatSuspendException(), Body(ran, "only")));
        Assert.Equal(1, t.Count);
        Assert.Equal(1u, t.Drain(true));
        Assert.Equal(new[] { "only" }, ran);
    }
}

public class EntryHostTests
{
    private sealed class Host { }

    private static EntryInvoker<Host> Returning(string json) => (_, _) => Task.FromResult(json);
    private static EntryInvoker<Host> Throwing(Exception e) => (_, _) => throw e;

    private static EntryTable<Host> One(EntryInvoker<Host> f) => new(new[] { ("E", f) });

    private static (Outcome o, List<string> log) Run(string args, EntryTable<Host> table, Action<DeferTable, List<string>>? setup = null)
    {
        var log = new List<string>();
        var defers = new DeferTable(on => log.Add(on ? "phase-on" : "phase-off"));
        setup?.Invoke(defers, log);
        return (EntryHost.Run(args, table, new Host(), defers), log);
    }

    [Fact]
    public void CompletedCarriesTheResult()
    {
        var (o, _) = Run("{}", One(Returning("{\"a\":1}")));
        Assert.Equal(OutcomeKind.Completed, o.Kind);
        Assert.Equal("{\"a\":1}", o.Text);
    }

    [Fact]
    public void SuspendedDoesNotDrainDefers()
    {
        var (o, log) = Run("{}", One(Throwing(new CleatSuspendException())),
            (d, l) => d.Register("x", () => { l.Add("defer-ran"); return Task.CompletedTask; }));
        Assert.Equal(OutcomeKind.Suspended, o.Kind);
        Assert.DoesNotContain("defer-ran", log);
    }

    [Fact]
    public void SuspendWrappedInAggregateStillSuspends()
    {
        // What `.Result` and `Task.WhenAll` produce.
        var (o, _) = Run("{}", One(Throwing(new AggregateException(new CleatSuspendException()))));
        Assert.Equal(OutcomeKind.Suspended, o.Kind);
    }

    [Fact]
    public void SuspendThrownFromAnAwaitedAsyncFrameReachesTheBoundary()
    {
        static async Task<int> Deep(int n) { await Task.CompletedTask; if (n == 0) throw new CleatSuspendException(); return 1 + await Deep(n - 1); }
        var (o, _) = Run("{}", One(async (_, _) => { await Deep(20); return "1"; }));
        Assert.Equal(OutcomeKind.Suspended, o.Kind);
    }

    [Fact]
    public void FailedDrainsDefersThenReportsTypeAndMessage()
    {
        var (o, log) = Run("{}", One(Throwing(new InvalidOperationException("boom"))),
            (d, l) => d.Register("x", () => { l.Add("defer-ran"); return Task.CompletedTask; }));
        Assert.Equal(OutcomeKind.Failed, o.Kind);
        Assert.Equal("InvalidOperationException: boom", o.Text);
        Assert.Equal(new[] { "phase-on", "defer-ran", "phase-off" }, log);
    }

    [Fact]
    public void FailedWithAnEmptyMessageStillNamesTheType()
    {
        var (o, _) = Run("{}", One(Throwing(new KeyNotFoundException(""))));
        Assert.Equal("KeyNotFoundException", o.Text);
    }

    [Fact]
    public void CompletedDrainsTheGuestsOwnDefersBeforeReporting()
    {
        var (o, log) = Run("{}", One(Returning("1")),
            (d, l) => d.Register("x", () => { l.Add("defer-ran"); return Task.CompletedTask; }));
        Assert.Equal(OutcomeKind.Completed, o.Kind);
        Assert.Equal(new[] { "phase-on", "defer-ran", "phase-off" }, log);
    }

    [Fact]
    public void ADeferThatSuspendsWinsOverTheResult()
    {
        var (o, _) = Run("{}", One(Returning("1")), (d, _) => d.Register("x", () => throw new CleatSuspendException()));
        Assert.Equal(OutcomeKind.Suspended, o.Kind);
    }

    [Fact]
    public void ADeferThatSuspendsOnTheFailurePathTurnsItIntoASuspension()
    {
        var (o, _) = Run("{}", One(Throwing(new InvalidOperationException("x"))), (d, _) => d.Register("x", () => throw new CleatSuspendException()));
        Assert.Equal(OutcomeKind.Suspended, o.Kind);
    }

    [Fact]
    public void RunDeferredIsTheHostsDrainAndSwallowsSuspends()
    {
        var log = new List<string>();
        var d = new DeferTable(_ => { });
        d.Register("a", () => { log.Add("a"); return Task.CompletedTask; });
        d.Register("b", () => throw new CleatSuspendException());
        Assert.Equal(2u, EntryHost.RunDeferred(d));
        Assert.Equal(new[] { "a" }, log);
    }

    // ---- input and dispatch ----------------------------------------------------------------------------------

    private static string? CapturedInput;
    private static EntryInvoker<Host> Capture(string name, List<string> hits) => (_, input) => { hits.Add(name); CapturedInput = input.GetRawText(); return Task.FromResult("null"); };

    [Fact]
    public void TheDispatchKeyPicksTheEntryAndIsRemovedFromTheInput()
    {
        var hits = new List<string>();
        var table = new EntryTable<Host>(new[] { ("A", Capture("A", hits)), ("B", Capture("B", hits)) });
        var (o, _) = Run("{\"__cleat_entry__\":\"B\",\"x\":1,\"y\":\"two\"}", table);
        Assert.Equal(OutcomeKind.Completed, o.Kind);
        Assert.Equal(new[] { "B" }, hits);
        Assert.Equal("{\"x\":1,\"y\":\"two\"}", CapturedInput);
    }

    [Fact]
    public void ASingleEntryNeedsNoKey()
    {
        var hits = new List<string>();
        var (o, _) = Run("{\"x\":1}", new EntryTable<Host>(new[] { ("A", Capture("A", hits)) }));
        Assert.Equal(OutcomeKind.Completed, o.Kind);
        Assert.Equal(new[] { "A" }, hits);
    }

    [Fact]
    public void SeveralEntriesWithoutAKeyFailAndNameTheEntries()
    {
        var hits = new List<string>();
        var table = new EntryTable<Host>(new[] { ("A", Capture("A", hits)), ("B", Capture("B", hits)) });
        var (o, _) = Run("{}", table);
        Assert.Equal(OutcomeKind.Failed, o.Kind);
        Assert.Contains("A, B", o.Text);
        Assert.Contains("__cleat_entry__", o.Text);
        Assert.Empty(hits);
    }

    [Fact]
    public void AnUnknownEntryFailsAndNamesIt()
    {
        var (o, _) = Run("{\"__cleat_entry__\":\"Nope\"}", One(Returning("1")));
        Assert.Equal(OutcomeKind.Failed, o.Kind);
        Assert.Contains("Nope", o.Text);
    }

    [Fact]
    public void EmptyInputIsAnEmptyObject()
    {
        var hits = new List<string>();
        Run("", new EntryTable<Host>(new[] { ("A", Capture("A", hits)) }));
        Assert.Equal("{}", CapturedInput);
    }

    [Fact]
    public void NumbersKeepTheExactDigitsTheyArrivedWith()
    {
        // 20 digits: not representable as a double or a long. Reformatting would corrupt it silently. Strings are
        // re-encoded (an escape may be spelled differently); the decoded value is what must survive.
        var hits = new List<string>();
        Run("{\"__cleat_entry__\":\"A\",\"big\":12345678901234567890,\"d\":1.50,\"s\":\"h\\u00e9\"}",
            new EntryTable<Host>(new[] { ("A", Capture("A", hits)) }));
        Assert.Equal("{\"big\":12345678901234567890,\"d\":1.50,\"s\":\"h\u00e9\"}", CapturedInput);
    }

    [Fact]
    public void MalformedInputIsAFailureNotACrash()
    {
        var (o, _) = Run("{not json", One(Returning("1")));
        Assert.Equal(OutcomeKind.Failed, o.Kind);
    }

    [Fact]
    public void ANonObjectInputIsPassedThrough()
    {
        var hits = new List<string>();
        Run("[1,2,3]", new EntryTable<Host>(new[] { ("A", Capture("A", hits)) }));
        Assert.Equal("[1,2,3]", CapturedInput);
    }

    [Fact]
    public void TwoEntriesWithOneNameAreRefused()
        => Assert.Throws<InvalidOperationException>(() => new EntryTable<Host>(new[] { ("A", Returning("1")), ("A", Returning("2")) }));
}

public class EntryJsonTests
{
    [Fact] public void StringIsAJsonString() => Assert.Equal("\"héllo \\\"q\\\"\"", EntryJson.String("héllo \"q\""));
    [Fact] public void NullStringIsNull() => Assert.Equal("null", EntryJson.String(null));
    [Fact] public void NumbersAndBools() { Assert.Equal("42", EntryJson.Int(42)); Assert.Equal("9223372036854775807", EntryJson.Long(long.MaxValue)); Assert.Equal("1.5", EntryJson.Double(1.5)); Assert.Equal("true", EntryJson.Bool(true)); }
    [Fact] public void ElementIsItsRawText() => Assert.Equal("{\"a\": 1}", EntryJson.Element(JsonDocument.Parse("{\"a\": 1}").RootElement));
    [Fact] public void NodeNullIsNull() => Assert.Equal("null", EntryJson.Node(null));
}

public class PackedHostResultTests
{
    private const ulong Stop = 1UL << 31;

    [Fact]
    public void ZeroIsSuccessAndTheUpperHalfIsReturned()
    {
        Assert.Equal(0u, Packed.CheckHostResult(0, "s", "o", "w"));
        Assert.Equal(7u, Packed.CheckHostResult(7UL << 32, "s", "o", "w"));
    }

    [Fact]
    public void TheStopIsTestedBeforeAnyField()
    {
        // The word carries an ERROR code too. If the error were read first this would be a call exception; the stop wins.
        Assert.Throws<CleatSuspendException>(() => Packed.CheckHostResult(Stop | 3, "s", "o", "w"));
        // And with a nonzero upper half, which a field-first decoder would return as an ordinary length.
        Assert.Throws<CleatSuspendException>(() => Packed.CheckHostResult((9UL << 32) | Stop, "s", "o", "w"));
    }

    [Theory]
    [InlineData(1u)]
    [InlineData(2u)]
    [InlineData(3u)]
    [InlineData(77u)]
    public void ALowByteErrorIsTheBaseExceptionWithNoClassification(uint code)
    {
        // Not a Timeout/Transient/Permanent subclass: the engine's code 1 here means "failed", not "timed out". A subclass
        // would hand a caller who retries timeouts a "promise not found" to retry.
        var e = Assert.ThrowsAny<CleatCallException>(() => Packed.CheckHostResult(code, "signal", "signal_workflow", "signal_workflow(x)"));
        Assert.Equal(typeof(CleatCallException), e.GetType());
        Assert.Equal(0u, e.Code);
        Assert.Equal("signal", e.Service);
        Assert.Contains("signal_workflow(x)", e.Message);
        Assert.Contains($"host status code {code}", e.Message);
        Assert.DoesNotContain("CallErrorTimeout", e.Message);
    }

    [Fact]
    public void OnlyTheLowByteIsTheErrorCode()
    {
        // 0x100 is a nonzero word whose low byte is zero: a length field, not an error.
        Assert.Equal(0u, Packed.CheckHostResult(0x100, "s", "o", "w"));
    }

    [Fact]
    public void SleepKeepsItsOwnWholeWordTest()
    {
        // A sleep word whose DURATION sets bit 31 is not a stop; the general decoder would say it is. They differ on purpose.
        var durationWithBit31 = (2UL << 31) | Stop;
        Assert.False(Packed.SleepMustSuspend(durationWithBit31));
        Assert.Throws<CleatSuspendException>(() => Packed.CheckHostResult(durationWithBit31, "s", "o", "w"));
    }
}

public class StringArrayJsonTests
{
    [Fact]
    public void AnEmptyListIsAnEmptyArray() => Assert.Equal("[]", EntryJson.StringArray(new string[0]));

    [Fact]
    public void ItemsKeepTheirOrderAndAreEscaped()
        => Assert.Equal("[\"b\",\"a\\\"q\",\"line\\nbreak\"]", EntryJson.StringArray(new[] { "b", "a\"q", "line\nbreak" }));

    [Fact]
    public void ANonAsciiNameIsWrittenLiterallyAndRoundTrips()
    {
        var json = EntryJson.StringArray(new[] { "caf\u00e9" });
        Assert.Contains("caf\u00e9", json);
        using var doc = JsonDocument.Parse(json);
        Assert.Equal("caf\u00e9", doc.RootElement[0].GetString());
    }
}

public class UpdateEnvelopeTests
{
    private static UpdateEnvelopeKind Decode(string raw, out string name, out string id, out string payload)
        => UpdateEnvelope.TryDecode(raw, out name, out id, out payload);

    [Fact]
    public void AnEmptyStringIsNothingPending()
        => Assert.Equal(UpdateEnvelopeKind.None, Decode("", out _, out _, out _));

    [Theory]
    [InlineData("not json")]
    [InlineData("[]")]
    [InlineData("\"just a string\"")]
    [InlineData("{\"request_id\":\"r\"}")]                           // no name
    [InlineData("{\"name\":\"n\"}")]                                  // no request id
    [InlineData("{\"name\":5,\"request_id\":\"r\"}")]                 // name is not a string
    [InlineData("{\"name\":\"n\",\"request_id\":null}")]              // request id is not a string
    [InlineData("{\"name\":\"\",\"request_id\":\"r\"}")]            // empty name
    [InlineData("{\"name\":\"n\",\"request_id\":\"\"}")]            // empty request id: complete_update would address nothing
    public void AnEnvelopeTheSdkCannotActOnIsMalformed(string raw)
        => Assert.Equal(UpdateEnvelopeKind.Malformed, Decode(raw, out _, out _, out _));

    [Fact]
    public void AGoodEnvelopeCarriesNamePayloadAndTheOpaqueRequestId()
    {
        Assert.Equal(UpdateEnvelopeKind.Update, Decode("{\"name\":\"n\",\"payload\":\"{\\\"a\\\":1}\",\"request_id\":\"n|row|p\"}", out var n, out var id, out var p));
        Assert.Equal("n", n);
        Assert.Equal("n|row|p", id);
        Assert.Equal("{\"a\":1}", p);
    }

    [Theory]
    [InlineData("{\"name\":\"n\",\"request_id\":\"r\"}")]                       // absent
    [InlineData("{\"name\":\"n\",\"request_id\":\"r\",\"payload\":5}")]         // not a string
    [InlineData("{\"name\":\"n\",\"request_id\":\"r\",\"payload\":null}")]
    public void ANonStringPayloadIsTheEmptyPayload(string raw)
    {
        Assert.Equal(UpdateEnvelopeKind.Update, Decode(raw, out _, out _, out var p));
        Assert.Equal("", p);
    }
}

public class UpdateDispatchTests
{
    private static string Env(string name, string payload, string id)
        => JsonSerializer.Serialize(new Dictionary<string, string> { ["name"] = name, ["payload"] = payload, ["request_id"] = id });

    /// <summary>A host that hands out the given envelopes in order, then "", and records every completion.</summary>
    private sealed class FakeHost
    {
        private readonly Queue<string> _queue;
        internal readonly List<(string Id, string Result, string Error)> Completed = new();
        internal int Polls;
        internal FakeHost(params string[] envelopes) { _queue = new Queue<string>(envelopes); }
        internal string Poll() { Polls++; return _queue.Count > 0 ? _queue.Dequeue() : ""; }
        internal void Complete(string id, string result, string error) => Completed.Add((id, result, error));
    }

    [Fact]
    public void ServesEveryPendingUpdateInOrderAndCompletesEachWithItsResult()
    {
        var u = new UpdateHandlers();
        u.Register("echo", p => "got:" + p, null);
        var h = new FakeHost(Env("echo", "a", "r1"), Env("echo", "b", "r2"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Equal(new[] { ("r1", "got:a", ""), ("r2", "got:b", "") }, h.Completed);
        Assert.Equal(3, h.Polls); // two updates, then the empty poll that ends it
    }

    [Fact]
    public void ANullResultIsAnEmptyResultAndStillResolves()
    {
        var u = new UpdateHandlers();
        u.Register("n", p => null!, null);
        var h = new FakeHost(Env("n", "", "r"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Equal(new[] { ("r", "", "") }, h.Completed);
    }

    [Fact]
    public void AnUnregisteredNameIsRejectedNotDropped()
    {
        var h = new FakeHost(Env("ghost", "", "r"));
        new UpdateHandlers().Dispatch(h.Poll, h.Complete);
        var c = Assert.Single(h.Completed);
        Assert.Equal("", c.Result);
        Assert.Contains("no update handler registered for 'ghost'", c.Error);
    }

    [Fact]
    public void AValidatorThatRefusesRejectsAndTheHandlerNeverRuns()
    {
        var u = new UpdateHandlers();
        var ran = false;
        u.Register("n", p => { ran = true; return "x"; }, p => false);
        var h = new FakeHost(Env("n", "bad", "r"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.False(ran);
        Assert.Equal("update 'n' failed validation", Assert.Single(h.Completed).Error);
    }

    [Fact]
    public void AValidatorThatThrowsAndAHandlerThatThrowsBothRejectWithTheMessage()
    {
        var u = new UpdateHandlers();
        u.Register("v", p => "x", p => throw new InvalidOperationException("validator blew up"));
        u.Register("h", p => throw new InvalidOperationException("handler blew up"), null);
        var h = new FakeHost(Env("v", "", "r1"), Env("h", "", "r2"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Equal(new[] { ("r1", "", "validator blew up"), ("r2", "", "handler blew up") }, h.Completed);
    }

    [Fact]
    public void AnExceptionWithAnEmptyMessageStillRejects()
    {
        // An empty error is what means "resolved", so an exception whose message is empty must not turn into a success.
        var u = new UpdateHandlers();
        u.Register("n", p => throw new InvalidOperationException(""), null);
        var h = new FakeHost(Env("n", "", "r"));
        u.Dispatch(h.Poll, h.Complete);
        var c = Assert.Single(h.Completed);
        Assert.NotEqual("", c.Error);
        Assert.Equal(nameof(InvalidOperationException), c.Error);
    }

    [Fact]
    public void AStopFromAHandlerPropagatesAndCompletesNothing()
    {
        var u = new UpdateHandlers();
        u.Register("n", p => throw new CleatSuspendException(), null);
        var h = new FakeHost(Env("n", "", "r"));
        Assert.Throws<CleatSuspendException>(() => u.Dispatch(h.Poll, h.Complete));
        Assert.Empty(h.Completed); // the update is redelivered next segment; answering it now would be an error for deferred work
    }

    [Fact]
    public void AStopFromAValidatorPropagatesToo()
    {
        var u = new UpdateHandlers();
        u.Register("n", p => "x", p => throw new CleatSuspendException());
        var h = new FakeHost(Env("n", "", "r"));
        Assert.Throws<CleatSuspendException>(() => u.Dispatch(h.Poll, h.Complete));
        Assert.Empty(h.Completed);
    }

    [Fact]
    public void ADispatchAfterAStopIsNotLockedOut()
    {
        var u = new UpdateHandlers();
        var first = true;
        u.Register("n", p => { if (first) { first = false; throw new CleatSuspendException(); } return "ok"; }, null);
        var h = new FakeHost(Env("n", "", "r"));
        Assert.Throws<CleatSuspendException>(() => u.Dispatch(h.Poll, h.Complete));
        var h2 = new FakeHost(Env("n", "", "r2"));
        u.Dispatch(h2.Poll, h2.Complete);
        Assert.Equal(new[] { ("r2", "ok", "") }, h2.Completed);
    }

    [Fact]
    public void ADispatchFromInsideAHandlerReturnsAtOnce()
    {
        var u = new UpdateHandlers();
        var inner = new FakeHost(Env("n", "", "inner"));
        u.Register("n", p => { u.Dispatch(inner.Poll, inner.Complete); return "outer-done"; }, null);
        var h = new FakeHost(Env("n", "", "r"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Equal(0, inner.Polls);      // the nested dispatch did not even poll
        Assert.Empty(inner.Completed);
        Assert.Equal(new[] { ("r", "outer-done", "") }, h.Completed);
    }

    [Fact]
    public void AMalformedEnvelopeEndsTheLoopWithoutCompletingOrSpinning()
    {
        var u = new UpdateHandlers();
        u.Register("n", p => "x", null);
        var h = new FakeHost("not json", Env("n", "", "never-reached"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Empty(h.Completed);
        Assert.Equal(1, h.Polls);
    }

    [Fact]
    public void ARegistrationReplacesAnEarlierOneOfTheSameName()
    {
        var u = new UpdateHandlers();
        u.Register("n", p => "old", null);
        u.Register("n", p => "new", null);
        var h = new FakeHost(Env("n", "", "r"));
        u.Dispatch(h.Poll, h.Complete);
        Assert.Equal("new", Assert.Single(h.Completed).Result);
    }
}

public class SignalEnvelopeTests
{
    private static bool Decode(string raw, out string reply, out string payload)
        => SignalEnvelope.TryDecode(raw, out reply, out payload);

    [Fact]
    public void ADecodesTheTwoKeyShape()
    {
        Assert.True(Decode("{\"cleat_reply_to\":\"promise-1\",\"payload\":\"{\\\"a\\\":1}\"}", out var r, out var p));
        Assert.Equal("promise-1", r);
        Assert.Equal("{\"a\":1}", p);
    }

    [Theory]
    [InlineData("{\"cleat_reply_to\":\"x\",\"payload\":\"p\",\"extra\":\"e\"}")]   // an ordinary payload that merely carries the key
    [InlineData("{\"cleat_reply_to\":\"x\"}")]
    [InlineData("{\"payload\":\"p\"}")]
    [InlineData("{\"cleat_reply_to\":\"\",\"payload\":\"p\"}")]                     // empty address
    [InlineData("{\"cleat_reply_to\":1,\"payload\":\"p\"}")]                          // not a string
    [InlineData("{\"cleat_reply_to\":\"x\",\"payload\":{\"a\":1}}")]                 // payload not a string
    [InlineData("{\"cleat_reply_to\":\"x\",\"cleat_reply_to\":\"y\"}")]              // a repeated key is not both keys
    [InlineData("[\"cleat_reply_to\",\"payload\"]")]
    [InlineData("not json")]
    [InlineData("")]
    public void EverythingElseIsAnOrdinaryPayload(string raw)
    {
        Assert.False(Decode(raw, out var r, out var p));
        Assert.Equal("", r);
        Assert.Equal("", p);
    }

    [Fact]
    public void APayloadThatOnlyMentionsTheKeyInATextFieldIsNotAnEnvelope()
    {
        // A substring search would call this one. The shape test does not.
        Assert.False(Decode("{\"note\":\"see cleat_reply_to\"}", out _, out _));
    }
}

public class ChildDocumentTests
{
    // ---- await_all_children ----

    [Fact]
    public void AllReadsEachResultAndKeepsTheRawError()
    {
        var r = ChildDocuments.ParseAll("[{\"run_id\":\"a\",\"result\":\"{}\"},{\"run_id\":\"b\",\"result\":\"\",\"error\":\"boom\"}]");
        Assert.Equal(2, r.Count);
        Assert.Equal(new ChildResult("a", "{}", null), r[0]);
        Assert.Equal(new ChildResult("b", "", "boom"), r[1]);
    }

    [Fact]
    public void AllReadsAnEmptyAnswerAsNoChildren()
        => Assert.Empty(ChildDocuments.ParseAll(""));

    [Theory]
    [InlineData("not json")]
    [InlineData("{\"run_id\":\"a\"}")]            // an object, not an array
    [InlineData("[{\"result\":\"x\"}]")]            // an item with no run_id
    [InlineData("[1]")]
    public void AllRefusesWhatIsNotTheDocumentItExpects(string raw)
    {
        var e = Assert.Throws<CleatCallException>(() => ChildDocuments.ParseAll(raw));
        Assert.Contains("await_all_children failed", e.Message);
    }

    // ---- await_any_child ----

    [Fact]
    public void AnyReadsTheWinner()
        => Assert.Equal(new ChildResult("w", "ok", null), ChildDocuments.ParseAny("{\"run_id\":\"w\",\"result\":\"ok\"}"));

    [Fact]
    public void AnyReadsAWinnerThatFailedAsAWinnerNotAsAHostError()
    {
        // BOTH run_id and error: the child failed, the call did not. Only an error with NO run_id is the host refusing.
        var r = ChildDocuments.ParseAny("{\"run_id\":\"w\",\"result\":\"\",\"error\":\"child blew up\"}");
        Assert.Equal(new ChildResult("w", "", "child blew up"), r);
    }

    [Fact]
    public void AnyThrowsOnAnErrorWithNoRunId()
    {
        var e = Assert.Throws<CleatCallException>(() => ChildDocuments.ParseAny("{\"error\":\"invalid runIDs: bad\"}"));
        Assert.Contains("invalid runIDs: bad", e.Message);
    }

    [Fact]
    public void AnyThrowsOnAnEmptyAnswerBecauseTheHostWritesTheWinnerThere()
    {
        var e = Assert.Throws<CleatCallException>(() => ChildDocuments.ParseAny(""));
        Assert.Contains("returned nothing", e.Message);
    }

    [Fact]
    public void AnyRefusesText()
        => Assert.Throws<CleatCallException>(() => ChildDocuments.ParseAny("no such child"));

    // ---- poll_child ----

    [Fact]
    public void PollIsNullWhenTheAnswerIsEmptyOrTheChildIsRunning()
    {
        Assert.Null(ChildDocuments.ParsePoll("r", ""));
        Assert.Null(ChildDocuments.ParsePoll("r", "{\"status\":\"running\"}"));
    }

    [Fact]
    public void PollTakesTheRunIdFromTheCallerBecauseTheDocumentHasNone()
    {
        var r = ChildDocuments.ParsePoll("run-9", "{\"status\":\"completed\",\"result\":\"42\"}");
        Assert.Equal(new ChildResult("run-9", "42", null), r);
    }

    [Fact]
    public void PollReadsAnEmptyErrorAsNoneButAnyOtherErrorAsAFailure()
    {
        Assert.Null(ChildDocuments.ParsePoll("r", "{\"status\":\"completed\",\"result\":\"x\",\"error\":\"\"}")!.Value.Error);
        Assert.Equal("nope", ChildDocuments.ParsePoll("r", "{\"status\":\"failed\",\"error\":\"nope\"}")!.Value.Error);
    }

    [Fact]
    public void PollRefusesWhatIsNotAnObject()
    {
        Assert.Throws<CleatCallException>(() => ChildDocuments.ParsePoll("r", "[1]"));
        Assert.Throws<CleatCallException>(() => ChildDocuments.ParsePoll("r", "garbage"));
    }
}

public class WaitDurationTests
{
    // cleat#4144. A positive wait must never reach the host as 0: AwaitSignalsMs reads 0 as "wait indefinitely".
    [Theory]
    [InlineData(5L, 1UL)]            // 5 ticks: positive and far under a millisecond
    [InlineData(10_000L, 1UL)]       // exactly 1 ms
    [InlineData(10_001L, 2UL)]       // just over: rounds up, not down
    [InlineData(15_000_000L, 1500UL)]
    public void APositiveSpanRoundsUpAndNeverToZero(long ticks, ulong want)
        => Assert.Equal(want, WaitDuration.Ms(TimeSpan.FromTicks(ticks)));

    [Fact]
    public void ZeroAndNegativeStayZero()
    {
        Assert.Equal(0UL, WaitDuration.Ms(TimeSpan.Zero));
        Assert.Equal(0UL, WaitDuration.Ms(TimeSpan.FromSeconds(-1)));
        Assert.Equal(0UL, WaitDuration.Ms(System.Threading.Timeout.InfiniteTimeSpan));
    }
}
