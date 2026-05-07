"""
Cleat SDK exceptions.
"""


class TerminalError(Exception):
    """A non-retryable error that terminates workflow execution immediately.

    In Restate, TerminalError signals that a failure is permanent and should
    not be retried. In Cleat, this serves the same purpose: when a workflow
    raises TerminalError, the runtime should not retry the failed step
    and should escalate to workflow-level error handling.

    Use this for:
    - Validation failures (negative amounts, missing fields)
    - Business rule violations
    - Any error where retrying would produce the same result
    """

    def __init__(self, message: str, status_code: int = 400):
        self.message = message
        self.status_code = status_code
        super().__init__(message)
