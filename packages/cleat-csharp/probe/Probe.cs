using System.Diagnostics;
using System.Runtime.CompilerServices;
using System.Security.Cryptography;
using System.Text.Json;
using System.Text.Json.Nodes;
using Cleat;

namespace Cleat.Probe;

/// <summary>
/// One entry point per API the determinism vet has a rule about. Each runs the API and reports what happened, as strings, so a test can
/// run an entry twice and say whether the values replay (the same workflow id and the same pinned clock must give the same answer) or
/// whether the API threw, and which exception. Nothing here is workflow code to copy: every entry is a deliberate violation.
/// </summary>
public static class Probe
{
    private static string Try(Func<object?> f)
    {
        try { return f()?.ToString() ?? "null"; }
        catch (CleatSuspendException) { throw; }
        catch (Exception e) { return "THREW " + e.GetType().FullName; }
    }

    private static JsonNode Report(params (string, string)[] fields)
    {
        var o = new JsonObject();
        foreach (var (k, v) in fields) o[k] = v;
        return o;
    }

    [CleatEntry("probe_clock")]
    public static JsonNode Clock(HostCalls h, JsonElement input) => Report(
        ("DateTime.UtcNow", Try(() => DateTime.UtcNow.Ticks)),
        ("DateTime.Now", Try(() => DateTime.Now.Ticks)),
        ("DateTimeOffset.UtcNow", Try(() => DateTimeOffset.UtcNow.ToUnixTimeMilliseconds())),
        ("TimeZoneInfo.Local", Try(() => TimeZoneInfo.Local.Id)),
        ("h.Now", Try(() => h.Now().Ticks)));

    [CleatEntry("probe_entropy")]
    public static JsonNode Entropy(HostCalls h, JsonElement input) => Report(
        ("Random.Shared", Try(() => Random.Shared.NextInt64())),
        ("new Random()", Try(() => new Random().NextInt64())),
        ("new Random(42)", Try(() => new Random(42).Next())),
        ("Guid.NewGuid", Try(() => Guid.NewGuid())),
        ("RandomNumberGenerator", Try(() => RandomNumberGenerator.GetInt32(1 << 30))),
        ("RuntimeHelpers.GetHashCode", Try(() => RuntimeHelpers.GetHashCode(new object()))),
        ("string.GetHashCode", Try(() => "abc".GetHashCode())),
        ("h.Random", Try(() => h.Random())));

    [CleatEntry("probe_monotonic")]
    public static JsonNode Monotonic(HostCalls h, JsonElement input) => Report(
        // Five readings, 7 ms apart (Thread.Sleep really sleeps, measured). A single millisecond reading is small and relative to the instance, so
        // a run and its replay can read the SAME value by coincidence: that happened once and flipped this row to "replays". Five spaced readings
        // are equal across runs only if the engine pins the clock, which is the question.
        ("Environment.TickCount64", Try(() =>
        {
            var reads = new long[5];
            for (var i = 0; i < reads.Length; i++) { reads[i] = Environment.TickCount64; Thread.Sleep(7); }
            return string.Join(",", reads);
        })),
        ("Stopwatch.GetTimestamp", Try(() => Stopwatch.GetTimestamp())));

    [CleatEntry("probe_thread_sleep")]
    public static JsonNode ThreadSleep(HostCalls h, JsonElement input)
    {
        var sw = Stopwatch.StartNew();
        var outcome = Try(() => { Thread.Sleep(50); return "returned"; });
        return Report(("Thread.Sleep(50)", outcome), ("elapsed-ms>=45", (sw.ElapsedMilliseconds >= 45).ToString()));
    }

    [CleatEntry("probe_tasks")]
    public static JsonNode Tasks(HostCalls h, JsonElement input) => Report(
        ("Task.Run", Try(() => Task.Run(() => 1).GetAwaiter().GetResult())),
        ("Task.Factory.StartNew", Try(() => Task.Factory.StartNew(() => 1).GetAwaiter().GetResult())),
        ("Task.Delay", Try(() => { Task.Delay(10).GetAwaiter().GetResult(); return "returned"; })),
        ("Task.Yield", Try(() => { Task.Yield().GetAwaiter().GetResult(); return "returned"; })),
        ("Task.Wait on an incomplete task", Try(() => { new TaskCompletionSource<int>().Task.Wait(); return "returned"; })),
        ("new Thread", Try(() => { var t = new Thread(() => { }); t.Start(); t.Join(); return "returned"; })),
        ("ThreadPool.QueueUserWorkItem", Try(() => ThreadPool.QueueUserWorkItem(_ => { }))));

    [CleatEntry("probe_async")]
    public static async Task<JsonNode> AsyncOverCalls(HostCalls h, JsonElement input)
    {
        var first = h.Call("probe", "first", "{}");
        await Task.CompletedTask;
        var second = await Task.FromResult(h.Call("probe", "second", "{}"));
        var both = await Task.WhenAll(Task.FromResult(1), Task.FromResult(2));
        var third = await Inner(h);
        return Report(("first", first), ("second", second), ("both", string.Join(",", both)), ("third", third));
    }

    private static async ValueTask<string> Inner(HostCalls h)
    {
        await Task.CompletedTask;
        return h.Call("probe", "third", "{}");
    }

    // The suspension unwinds as CleatSuspendException, so a catch that swallows it lets the workflow carry on past a call the host told it
    // to stop at. Each entry sleeps for a minute (a real suspension on a fresh run) inside the catch under test, then makes a host call
    // AFTER the catch. The test reports how many host calls the SERVICE received and whether the engine saw a suspension: a call after the
    // catch that reaches the service is work done past the suspension point.
    [CleatEntry("probe_swallow_exception")]
    public static JsonNode SwallowException(HostCalls h, JsonElement input)
    {
        try { h.Sleep(TimeSpan.FromSeconds(60)); }
        catch (Exception) { }
        h.Call("probe", "after_swallow", "{}");
        return Report(("path", "after-the-catch"));
    }

    [CleatEntry("probe_swallow_bare")]
    public static JsonNode SwallowBare(HostCalls h, JsonElement input)
    {
        try { h.Sleep(TimeSpan.FromSeconds(60)); }
        catch { }
        h.Call("probe", "after_swallow", "{}");
        return Report(("path", "after-the-catch"));
    }

    [CleatEntry("probe_swallow_filtered")]
    public static JsonNode SwallowFiltered(HostCalls h, JsonElement input)
    {
        try { h.Sleep(TimeSpan.FromSeconds(60)); }
        catch (Exception e) when (e is not CleatSuspendException) { }
        h.Call("probe", "after_swallow", "{}");
        return Report(("path", "after-the-catch"));
    }

    [CleatEntry("probe_swallow_rethrown")]
    public static JsonNode SwallowRethrown(HostCalls h, JsonElement input)
    {
        try { h.Sleep(TimeSpan.FromSeconds(60)); }
        catch (Exception) { throw; }
        h.Call("probe", "after_swallow", "{}");
        return Report(("path", "after-the-catch"));
    }

    [CleatEntry("probe_io")]
    public static JsonNode Io(HostCalls h, JsonElement input) => Report(
        ("File.Exists", Try(() => File.Exists("/etc/passwd"))),
        ("File.ReadAllText", Try(() => File.ReadAllText("/etc/hostname").Length)),
        ("File.WriteAllText", Try(() => { File.WriteAllText("/tmp/cleat-probe", "x"); return "returned"; })),
        ("Directory.GetCurrentDirectory", Try(() => Directory.GetCurrentDirectory())),
        ("Console.WriteLine", Try(() => { Console.WriteLine("probe"); return "returned"; })),
        ("Console.ReadLine", Try(() => Console.ReadLine() ?? "null")));
    // HttpClient is NOT here, deliberately: a component that references it imports wasi:http/types, which the engine does not provide, so
    // EVERY entry of the component fails to instantiate (measured, cleat#4080 PR 6: "component imports instance wasi:http/types@0.2.0,
    // but a matching implementation was not found in the linker"). An entry here would make the whole probe unrunnable.

    [CleatEntry("probe_environment")]
    public static JsonNode EnvironmentProbe(HostCalls h, JsonElement input) => Report(
        ("GetEnvironmentVariable(PATH)", Try(() => Environment.GetEnvironmentVariable("PATH"))),
        ("MachineName", Try(() => Environment.MachineName)),
        ("ProcessId", Try(() => Environment.ProcessId)),
        ("UserName", Try(() => Environment.UserName)),
        ("CurrentDirectory", Try(() => Environment.CurrentDirectory)),
        ("OSVersion", Try(() => Environment.OSVersion.ToString())));
}
