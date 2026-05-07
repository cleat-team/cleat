package cleatexample.statemachine.types;

/**
 * Represents the lifecycle status of a payment.
 * Stored as a plain string in Cleat query state since enum support
 * in JsonHelper relies on field-based POJO mapping.
 */
public enum PaymentStatus {
    NEW,
    COMPLETED_SUCCESSFULLY,
    CANCELLED
}
