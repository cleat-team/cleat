use crate::host_calls::HostCalls;

/// A single step in a Saga: a forward operation and its compensation.
#[allow(clippy::type_complexity)]
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
                            comp_errs.push(format!("compensation cleanup failed for step '{}': {}", self.steps[j].name, ce));
                        }
                    }
                    if comp_errs.is_empty() {
                        return Err(e);
                    }
                    return Err(format!("{} (forward error; compensation cleanup errors: {})", e, comp_errs.join("; ")));
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;

    // Test 1: All steps succeed, no compensation needed
    #[test]
    fn test_saga_all_success() {
        let saga = Saga::new()
            .add_step("step1", |_h| Ok("result1".to_string()), |_h| Ok(()))
            .add_step("step2", |_h| Ok("result2".to_string()), |_h| Ok(()));
        let result = saga.run(&HostCalls);
        assert!(result.is_ok());
    }

    // Test 2: Compensation on failure — step2 fails, step1 should be compensated
    #[test]
    fn test_saga_compensation_on_failure() {
        use std::sync::atomic::{AtomicBool, Ordering};
        let compensated = Arc::new(AtomicBool::new(false));
        let compensated_comp = compensated.clone();

        let saga = Saga::new()
            .add_step("step1", |_h| Ok("result1".to_string()), move |_h| {
                compensated_comp.store(true, Ordering::SeqCst);
                Ok(())
            })
            .add_step("step2", |_h| Err("step2 failed".to_string()), |_h| Ok(()));

        let result = saga.run(&HostCalls);
        assert!(result.is_err());
        assert!(compensated.load(Ordering::SeqCst), "step1 should have been compensated");
    }

    // Test 3: Multi-step ordering — verify LIFO compensation order
    #[test]
    fn test_saga_multi_step_ordering() {
        use std::sync::{Arc, Mutex};
        let order = Arc::new(Mutex::new(Vec::new()));
        let f1 = order.clone();
        let f2 = order.clone();
        let f3 = order.clone();
        let c1 = order.clone();
        let c2 = order.clone();
        let c3 = order.clone();

        let saga = Saga::new()
            .add_step("step1", move |_h| {
                f1.lock().unwrap().push("forward1");
                Ok("result1".to_string())
            }, move |_h| {
                c1.lock().unwrap().push("compensate1");
                Ok(())
            })
            .add_step("step2", move |_h| {
                f2.lock().unwrap().push("forward2");
                Ok("result2".to_string())
            }, move |_h| {
                c2.lock().unwrap().push("compensate2");
                Ok(())
            })
            .add_step("step3", move |_h| {
                f3.lock().unwrap().push("forward3");
                Err("step3 failed".to_string())
            }, move |_h| {
                c3.lock().unwrap().push("compensate3");
                Ok(())
            });

        let result = saga.run(&HostCalls);
        assert!(result.is_err());

        let order = order.lock().unwrap();
        assert_eq!(&order[..3], &["forward1", "forward2", "forward3"]);
        // Compensation should be LIFO: step2 then step1 (NOT step3 since it failed)
        assert_eq!(&order[3..], &["compensate2", "compensate1"]);
    }

    // Test 4: Error in compensation
    #[test]
    fn test_saga_error_in_compensation() {
        let saga = Saga::new()
            .add_step("step1", |_h| Ok("ok".to_string()), |_h| Err("compensation failed".to_string()))
            .add_step("step2", |_h| Err("forward failed".to_string()), |_h| Ok(()));

        let result = saga.run(&HostCalls);
        assert!(result.is_err());
        let err = result.unwrap_err();
        assert!(err.contains("forward failed"));
        assert!(err.contains("compensation cleanup errors"));
    }
}
