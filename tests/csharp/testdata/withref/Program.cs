using CleatWorkflowWorld.wit.Imports.cleat.hostCalls;

namespace CleatWorkflowWorld;

// The spike's hello (cleat#4077): one durable-identity call and one durable call, mapped to run-outcome by hand.
public class CleatWorkflowWorldExportsImpl : ICleatWorkflowWorldExports
{
    public static ICleatWorkflowWorldImports.RunOutcome Run(string args)
    {
        try
        {
            var id = IDurableIdentityImports.DurableWorkflowId();
            var reply = IDurableCallImports.DurableCall("payments", "Charge", args);
            return ICleatWorkflowWorldImports.RunOutcome.Completed("{\"hello\":\"" + id + "\",\"reply\":" + reply + "}");
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

    public static uint RunDeferred() => 0;
}
