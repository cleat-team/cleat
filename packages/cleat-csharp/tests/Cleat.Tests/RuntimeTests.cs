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
