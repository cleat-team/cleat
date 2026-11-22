using System.Diagnostics;
using System.Text;
using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace CleatWorkflowWorld;

sealed class SuspendException : Exception { }

public class CleatWorkflowWorldExportsImpl : ICleatWorkflowWorldExports
{
    static readonly List<byte[]> Keep = new();
    static volatile bool Stop;
    static bool SkipPhase;
    static readonly List<(string id, Action body)> Defers = new();

    // Registers a defer the way an SDK must since cleat#4079: the body goes into the table BEFORE the host call, because durable-defer
    // can answer err(suspended) -- a killed run cut short at this very registration -- and that stop carries no id, while the drain that
    // follows the unwind still has to find the body. On ok the entry gets the host's id; on a failure that is not a stop it is removed.
    static void RegisterDefer(string description, Action body)
    {
        var entry = (id: "", body);
        Defers.Add(entry);
        string id;
        try { id = IDurableLifecycleImports.DurableDefer(description); }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f && f.Tag != IOutcomesImports.CallFailure.Tags.Suspended)
        {
            Defers.Remove(entry);
            throw;
        }
        var i = Defers.IndexOf(entry);
        if (i >= 0) Defers[i] = (id, body);
    }

    static string Field(string a, string key, string dflt)
    {
        var k = "\"" + key + "\":\"";
        var i = a.IndexOf(k, StringComparison.Ordinal);
        if (i < 0) return dflt;
        i += k.Length;
        var j = a.IndexOf('"', i);
        return a.Substring(i, j - i);
    }

    static void Sleep(ulong ms)
    {
        var r = IDurableSleepImports.DurableSleep(ms);
        if ((r >> 56) == 1) throw new SuspendException();   // engine said: suspend
    }

    static int Deep(int n, ulong ms)
    {
        if (n == 0) { Sleep(ms); return 0; }
        return 1 + Deep(n - 1, ms);                          // exception unwinds through N frames
    }

    public static ICleatWorkflowWorldImports.RunOutcome Run(string args)
    {
        try
        {
            switch (Field(args, "mode", "hello"))
            {
                case "hello":
                {
                    var id = IDurableIdentityImports.DurableWorkflowId();
                    var reply = IDurableCallImports.DurableCall("payments", "Charge", args);
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"id\":\"" + id + "\",\"reply\":" + reply + "}");
                }
                case "sleep":
                {
                    var first = IDurableCallImports.DurableCall("payments", "Charge", args);   // event 0: anchors the sleep
                    Deep(20, 400);                           // suspends first run, completes on replay
                    var reply = IDurableCallImports.DurableCall("payments", "Charge", args);
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"first\":" + first + ",\"after_sleep\":" + reply + "}");
                }
                case "det2":
                {
                    var anchor = IDurableCallImports.DurableCall("payments", "Charge", args);   // event 0 anchors the durable clock
                    var s2 = "{\"utcnow_ticks\":" + DateTime.UtcNow.Ticks +
                             ",\"now_ms\":" + DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() +
                             ",\"rnd\":" + Random.Shared.Next() +
                             ",\"rnd2\":" + new Random().Next() +
                             ",\"guid\":\"" + Guid.NewGuid() + "\"" +
                             ",\"tick64\":" + Environment.TickCount64 +
                             ",\"durable_now\":" + IDurableSleepImports.DurableNow() +
                             ",\"durable_rnd\":" + IDurableSleepImports.DurableRandom() + "}";
                    return ICleatWorkflowWorldImports.RunOutcome.Completed(s2);
                }
                case "det":
                {
                    string task;
                    try { task = Task.Run(() => 42).Result.ToString(); } catch (Exception e) { task = "EXC:" + e.GetType().Name; }
                    var sw = Stopwatch.StartNew();
                    var s = "{\"utcnow_ticks\":" + DateTime.UtcNow.Ticks +
                            ",\"now_ms\":" + DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() +
                            ",\"rnd\":" + Random.Shared.Next() +
                            ",\"guid\":\"" + Guid.NewGuid() + "\"" +
                            ",\"task\":\"" + task + "\"" +
                            ",\"tick64\":" + Environment.TickCount64 +
                            ",\"sw_ticks\":" + (sw.ElapsedTicks >= 0 ? 1 : 0) + "}";
                    return ICleatWorkflowWorldImports.RunOutcome.Completed(s);
                }
                case "heldmem":
                {
                    // MEASUREMENT mode (cleat#4377): `calls` durable calls, `mb` MiB of touched live data, then EITHER a durable sleep of `sleep_ms` OR one blocking call named park.
                    var calls = int.Parse(Field(args, "calls", "0"));
                    var mb = int.Parse(Field(args, "mb", "0"));
                    var sleepMs = ulong.Parse(Field(args, "sleep_ms", "0"));
                    for (int i = 0; i < calls; i++) IDurableCallImports.DurableCall("work", "step", "{\"stage\":\"step\"}");
                    for (int i = 0; i < mb; i++)
                    {
                        var blk = new byte[1024 * 1024];
                        for (int j = 0; j < blk.Length; j += 4096) blk[j] = 1;
                        Keep.Add(blk);
                    }
                    var gi = GC.GetGCMemoryInfo();
                    IDurableCallImports.DurableCall("work", "info", "{\"heap_size\":" + gi.HeapSizeBytes + ",\"committed\":" + gi.TotalCommittedBytes + ",\"available\":" + gi.TotalAvailableMemoryBytes
                        + ",\"total_memory\":" + GC.GetTotalMemory(false) + ",\"server_gc\":" + (System.Runtime.GCSettings.IsServerGC ? 1 : 0) + ",\"gen0_size\":" + (gi.GenerationInfo.Length > 0 ? gi.GenerationInfo[0].SizeAfterBytes : -1)
                        + ",\"fragmented\":" + gi.FragmentedBytes + ",\"gen0_collections\":" + GC.CollectionCount(0) + "}");
                    GC.Collect();
                    var g2 = GC.GetGCMemoryInfo();
                    IDurableCallImports.DurableCall("work", "info2", "{\"after_gc_heap_size\":" + g2.HeapSizeBytes + ",\"after_gc_committed\":" + g2.TotalCommittedBytes + ",\"after_gc_available\":" + g2.TotalAvailableMemoryBytes
                        + ",\"after_gc_total_memory\":" + GC.GetTotalMemory(false) + ",\"gen0_after\":" + (g2.GenerationInfo.Length > 0 ? g2.GenerationInfo[0].SizeAfterBytes : -1) + ",\"gen2_after\":" + (g2.GenerationInfo.Length > 2 ? g2.GenerationInfo[2].SizeAfterBytes : -1)
                        + ",\"knob_HeapHardLimit\":\"" + (AppContext.GetData("System.GC.HeapHardLimit")?.ToString() ?? "null") + "\",\"knob_GCHeapHardLimit\":\"" + (AppContext.GetData("GCHeapHardLimit")?.ToString() ?? "null") + "\"}");
                    if (sleepMs > 0) Sleep(sleepMs);
                    else IDurableCallImports.DurableCall("work", "park", "{\"stage\":\"park\"}");
                    IDurableCallImports.DurableCall("work", "done", "{\"stage\":\"done\"}");
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"kept\":" + Keep.Count + "}");
                }
                case "alloc":
                {
                    var mb = int.Parse(Field(args, "mb", "10"));
                    var keep = new List<byte[]>();
                    long sum = 0;
                    for (int i = 0; i < mb; i++)
                    {
                        var b = new byte[1024 * 1024];
                        b[0] = (byte)i; b[^1] = 7;
                        keep.Add(b);
                        sum += b[0] + b[^1];
                    }
                    GC.Collect();
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"mb\":" + mb + ",\"sum\":" + sum + ",\"input_len\":" + args.Length + ",\"gc_total\":" + GC.GetTotalMemory(false) + "}");
                }
                case "exc":
                {
                    string r;
                    try { throw new InvalidOperationException("boom"); } catch (InvalidOperationException e) { r = e.Message; }
                    try { _ = int.Parse("nope"); } catch (FormatException) { r += "+fmt"; }
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"caught\":\"" + r + "\"}");
                }
                case "defer":
                {
                    RegisterDefer("refund", () => IDurableCallImports.DurableCall("payments", "Refund", "{\"x\":1}"));
                    Deep(3, 400);                            // suspends: the defer must then run on this instance
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"done\":true}");
                }
                case "spin":
                {
                    SkipPhase = Field(args, "phase", "1") == "0";
                    RegisterDefer("refund", () => IDurableCallImports.DurableCall("payments", "Refund", "{\"x\":1}"));
                    long x = 0;
                    while (!Stop) { x++; }                   // killed by the epoch fence; Stop is volatile so this cannot be folded
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"never\":true}");
                }
                case "defer_order":
                {
                    // Models what an SDK's entry wrapper does: register a defer, make the body's own call, and drain the
                    // defers when the body completes. In a defer segment the body call is refused (suspended), so the
                    // body never completes and the HOST drains through run-deferred instead.
                    RegisterDefer("refund", () => IDurableCallImports.DurableCall("payments", "Refund", "{}"));
                    IDurableCallImports.DurableCall("payments", "Body", "{}");
                    RunDeferred();
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"done\":true}");
                }
                case "poll":
                {
                    var c = IDurableLifecycleImports.DurablePollCancellation();
                    return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"cancel\":\"" + c + "\"}");
                }
                default:
                    return ICleatWorkflowWorldImports.RunOutcome.Failed("unknown mode");
            }
        }
        catch (SuspendException)
        {
            return ICleatWorkflowWorldImports.RunOutcome.Suspended();
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f && f.Tag == IOutcomesImports.CallFailure.Tags.Suspended)
        {
            return ICleatWorkflowWorldImports.RunOutcome.Suspended();
        }
        catch (WitException e) when (e.Value is IOutcomesImports.CallFailure f)
        {
            return ICleatWorkflowWorldImports.RunOutcome.Failed(f.AsFailed.message);
        }
    }

    public static uint RunDeferred()
    {
        uint n = 0;
        if (!SkipPhase) IDurableLifecycleImports.DurableDeferPhase(true);
        try
        {
            var table = Defers.ToArray();
            Defers.Clear();
            foreach (var (_, body) in table) { body(); n++; }
        }
        finally { if (!SkipPhase) IDurableLifecycleImports.DurableDeferPhase(false); }
        return n;
    }
}
