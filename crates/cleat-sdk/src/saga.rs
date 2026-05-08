use crate::host_calls::HostCalls;

/// A single step in a Saga: a forward operation and its compensation.
pub struct SagaStep {
    pub name: String,
    forward: Box<dyn Fn(&HostCalls) -> Result<String, String>>,
    compensate: Box<dyn Fn(&HostCalls) -> Result<(), String>>,
}

impl SagaStep {
    pub fn new(
        name: impl Into<String>,
        forward: impl Fn(&HostCalls) -> Result<String, String> + 'static,
        compensate: impl Fn(&HostCalls) -> Result<(), String> + 'static,
    ) -> Self {
        SagaStep {
            name: name.into(),
            forward: Box::new(forward),
            compensate: Box::new(compensate),
        }
    }
}

/// A Saga executes steps sequentially. If any step fails, completed steps
/// are compensated in reverse (LIFO) order.
pub struct Saga {
    steps: Vec<SagaStep>,
}

impl Saga {
    pub fn new() -> Self {
        Saga { steps: Vec::new() }
    }

    pub fn add_step(
        mut self,
        name: impl Into<String>,
        forward: impl Fn(&HostCalls) -> Result<String, String> + 'static,
        compensate: impl Fn(&HostCalls) -> Result<(), String> + 'static,
    ) -> Self {
        self.steps.push(SagaStep::new(name, forward, compensate));
        self
    }

    /// Run all steps. On any error, compensate completed steps in reverse order.
    pub fn run(&self, h: &HostCalls) -> Result<(), String> {
        let mut completed: Vec<usize> = Vec::new();
        for (i, step) in self.steps.iter().enumerate() {
            match (step.forward)(h) {
                Ok(_) => completed.push(i),
                Err(e) => {
                    // Compensate completed steps in LIFO order.
                    let mut comp_errs: Vec<String> = Vec::new();
                    for &j in completed.iter().rev() {
                        if let Err(ce) = (self.steps[j].compensate)(h) {
                            comp_errs.push(format!("{}: {}", self.steps[j].name, ce));
                        }
                    }
                    if comp_errs.is_empty() {
                        return Err(e);
                    }
                    return Err(format!("{} (compensation errors: {})", e, comp_errs.join("; ")));
                }
            }
        }
        Ok(())
    }
}

impl Default for Saga {
    fn default() -> Self {
        Self::new()
    }
}
