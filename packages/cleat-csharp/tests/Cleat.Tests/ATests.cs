using Cleat;
using Cleat.Runtime;
using Xunit;

namespace Cleat.Tests;

// Group A (cleat#4080): the binding-independent half. HostCalls itself is excluded from this project (it sits on the generated bindings),
// so what can be checked here is the policy's wire encoding and the defer-phase flag; the calls themselves are driven through the real
// engine by tests/csharp/group_a_test.go.

public class RetryPolicyWireTests
{
    [Fact]
    public void DefaultsAreThePythonSdksDefaults()
    {
        var w = new RetryPolicy().ToWire();
        Assert.Equal(3UL, w.MaxAttempts);
        Assert.Equal(1000UL, w.InitialIntervalMs);
        Assert.Equal(200UL, w.BackoffCoefficient100x);
        Assert.Equal(30000UL, w.MaxIntervalMs);
        Assert.Equal("[]", w.NonRetryableErrorsJson);
    }

    [Fact]
    public void ValuesReachTheWireInTheHostsUnits()
    {
        var w = new RetryPolicy
        {
            MaxAttempts = 7,
            InitialInterval = TimeSpan.FromMilliseconds(250),
            BackoffCoefficient = 1.5,
            MaxInterval = TimeSpan.FromSeconds(2),
            NonRetryableErrors = new[] { "INSUFFICIENT_FUNDS", "CARD_DECLINED" },
        }.ToWire();
        Assert.Equal(7UL, w.MaxAttempts);
        Assert.Equal(250UL, w.InitialIntervalMs);
        Assert.Equal(150UL, w.BackoffCoefficient100x);
        Assert.Equal(2000UL, w.MaxIntervalMs);
        Assert.Equal("[\"INSUFFICIENT_FUNDS\",\"CARD_DECLINED\"]", w.NonRetryableErrorsJson);
    }

    // Python's round() is half-to-even for the same conversion: 1.005 * 100 is 100.49999999999999, and 2.675 * 100 is 267.49999999999994,
    // so neither is a midpoint; an exact .5 hundredth (0.125 -> 12.5) is.
    [Theory]
    [InlineData(0.125, 12UL)]
    [InlineData(0.135, 14UL)]
    [InlineData(1.0, 100UL)]
    [InlineData(0.0, 0UL)]
    public void CoefficientRoundsHalfToEven(double coefficient, ulong want)
        => Assert.Equal(want, new RetryPolicy { BackoffCoefficient = coefficient }.ToWire().BackoffCoefficient100x);

    [Fact]
    public void NonRetryableCodesAreEscapedAsJson()
    {
        var json = RetryPolicy.EncodeNonRetryable(new[] { "a\"b", "c\\d", "e\nf" });
        using var doc = System.Text.Json.JsonDocument.Parse(json);
        Assert.Equal(new[] { "a\"b", "c\\d", "e\nf" }, doc.RootElement.EnumerateArray().Select(x => x.GetString()).ToArray());
    }

    [Fact]
    public void NoCodesIsAnEmptyArrayNotAnEmptyString() => Assert.Equal("[]", RetryPolicy.EncodeNonRetryable(null));

    [Fact]
    public void ANullCodeIsRefusedNotWrittenAsNull()
        => Assert.Throws<ArgumentException>(() => RetryPolicy.EncodeNonRetryable(new string[] { null! }));

    [Fact] public void ZeroAttemptsIsRefused() => Assert.Throws<ArgumentOutOfRangeException>(() => new RetryPolicy { MaxAttempts = 0 }.ToWire());
    [Fact] public void NegativeAttemptsIsRefused() => Assert.Throws<ArgumentOutOfRangeException>(() => new RetryPolicy { MaxAttempts = -1 }.ToWire());
    [Fact] public void OneAttemptIsAPolicy() => Assert.Equal(1UL, new RetryPolicy { MaxAttempts = 1 }.ToWire().MaxAttempts);

    [Fact]
    public void NegativeIntervalsAreRefused()
    {
        Assert.Throws<ArgumentOutOfRangeException>(() => new RetryPolicy { InitialInterval = TimeSpan.FromMilliseconds(-1) }.ToWire());
        Assert.Throws<ArgumentOutOfRangeException>(() => new RetryPolicy { MaxInterval = TimeSpan.FromMilliseconds(-1) }.ToWire());
    }

    // Zero is a legitimate MaxInterval: the host reads 0 as "no cap".
    [Fact] public void ZeroMaxIntervalMeansNoCapAndIsAccepted() => Assert.Equal(0UL, new RetryPolicy { MaxInterval = TimeSpan.Zero }.ToWire().MaxIntervalMs);

    [Theory]
    [InlineData(double.NaN)]
    [InlineData(double.PositiveInfinity)]
    [InlineData(double.NegativeInfinity)]
    [InlineData(-0.5)]
    [InlineData(1e300)]
    public void ACoefficientThatCannotBeHundredthsIsRefused(double coefficient)
        => Assert.Throws<ArgumentOutOfRangeException>(() => new RetryPolicy { BackoffCoefficient = coefficient }.ToWire());

    [Fact] public void TheRefusedPolicyCodeIsTheHostsCode() => Assert.Equal(6u, RetryPolicy.RetryPolicyTooLongCode);
}

public class DeferPhaseFlagTests
{
    private static DeferTable Table() => new(_ => { });

    [Fact] public void NotInAPhaseBeforeADrain() => Assert.False(Table().InDeferPhase);

    [Fact]
    public void InAPhaseWhileABodyRunsAndNotAfter()
    {
        var t = Table();
        var during = false;
        t.Register("a", () => { during = t.InDeferPhase; return Task.CompletedTask; });
        t.Drain(true);
        Assert.True(during);
        Assert.False(t.InDeferPhase);
    }

    [Fact]
    public void NotInAPhaseAfterABodyThatThrows()
    {
        var t = Table();
        t.Register("a", () => throw new InvalidOperationException("boom"));
        t.Drain(true);
        Assert.False(t.InDeferPhase);
    }

    [Fact]
    public void NotInAPhaseAfterASuspendThatPropagates()
    {
        var t = Table();
        t.Register("a", () => throw new CleatSuspendException());
        Assert.Throws<CleatSuspendException>(() => t.Drain(propagateSuspend: true));
        Assert.False(t.InDeferPhase);
    }

    // An empty table makes no phase calls (DeferTable's documented difference from Python), so the flag stays down too.
    [Fact]
    public void AnEmptyDrainNeverRaisesTheFlag()
    {
        var phases = new List<bool>();
        var t = new DeferTable(on => phases.Add(on));
        t.Drain(true);
        Assert.Empty(phases);
        Assert.False(t.InDeferPhase);
    }

    // The flag is the SAME fact the host is told: it is up exactly between phase(true) and phase(false).
    [Fact]
    public void TheFlagIsUpExactlyWhileTheHostIsToldThePhaseIsOn()
    {
        var seen = new List<string>();
        DeferTable? t = null;
        t = new DeferTable(on => seen.Add($"phase({on}) flag={t!.InDeferPhase}"));
        t.Register("a", () => { seen.Add($"body flag={t.InDeferPhase}"); return Task.CompletedTask; });
        t.Drain(true);
        // phase(true) is called before the flag rises and phase(false) after it falls, so the host's phase brackets the flag.
        Assert.Equal(new[] { "phase(True) flag=False", "body flag=True", "phase(False) flag=False" }, seen);
    }
}
