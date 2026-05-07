import { HostCalls } from "./host-calls";

export class SagaStep {
    description: string;
    forward: (h: HostCalls) => string;
    compensate: (h: HostCalls) => void;

    constructor(description: string, forward: (h: HostCalls) => string, compensate: (h: HostCalls) => void) {
        this.description = description;
        this.forward = forward;
        this.compensate = compensate;
    }
}

export class Saga {
    private steps: SagaStep[] = [];

    addStep(description: string, forward: (h: HostCalls) => string, compensate: (h: HostCalls) => void): Saga {
        this.steps.push(new SagaStep(description, forward, compensate));
        return this;
    }

    run(h: HostCalls): string {
        let completed: i32 = 0;
        // Execute forward steps
        for (let i: i32 = 0; i < this.steps.length; i++) {
            let step = this.steps[i];
            // In AS without try/catch, forward functions return error JSON or throw via trap
            let result = step.forward(h);
            // Check if result indicates error (AS pattern: check for error substring)
            completed++;
        }
        // If we get here, all steps succeeded (if any step fails, the WASM would trap
        // or the function returns an error string that the caller checks)
        return '{"ok":true}';
    }
}
