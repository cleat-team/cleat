package com.cleat.example;

import cleat.HostCalls;
import cleat.CleatEntry;
import cleat.CleatResult;

/**
 * Example order processing workflow using the cleat Java SDK.
 *
 * This workflow implements a Saga-like compensation pattern:
 * <ol>
 *   <li>Reserves inventory</li>
 *   <li>Charges payment (with compensation on failure)</li>
 *   <li>Creates shipment (with compensation on failure)</li>
 *   <li>Sends notification (best-effort, no compensation needed)</li>
 * </ol>
 *
 * Each step that modifies state has a compensating action that is invoked
 * if a subsequent step fails, ensuring eventual consistency.
 */
public class PlaceOrder {

    /**
     * Main order placement workflow entry point.
     * <p>
     * Accepts a JSON order input and orchestrates inventory reservation,
     * payment processing, shipment creation, and notification.
     *
     * @param h     the {@link HostCalls} instance for durable orchestration
     * @param input the JSON-encoded order input string
     * @return a JSON result string indicating success or failure
     */
    @CleatEntry(name = "place_order")
    public static String placeOrder(HostCalls h, String input) {
        // Validate input
        if (input == null || input.isEmpty() || input.equals("{}")) {
            return "{\"error\":\"empty order input\"}";
        }

        // Step 1: Reserve inventory
        String reserveReq = "{\"reservation\":" + input + "}";
        CleatResult<String> reserveResult = h.cleatCall("inventory", "Reserve", reserveReq);
        if (reserveResult.isErr()) {
            return "{\"error\":\"inventory reserve failed: " + escapeJSON(reserveResult.getError()) + "\"}";
        }
        String reservationJSON = reserveResult.getValue();

        // Step 2: Charge payment
        String chargeReq = "{\"charge\":" + input + "}";
        CleatResult<String> chargeResult = h.cleatCall("payments", "Charge", chargeReq);
        if (chargeResult.isErr()) {
            // Compensate: release inventory
            String releaseReq = "{\"release\":" + reservationJSON + "}";
            h.cleatCall("inventory", "Release", releaseReq);
            return "{\"error\":\"payment failed: " + escapeJSON(chargeResult.getError()) + "\"}";
        }

        // Step 3: Create shipment
        String shipReq = "{\"shipment\":" + input + ",\"reservation\":" + reservationJSON + "}";
        CleatResult<String> shipResult = h.cleatCall("shipping", "CreateShipment", shipReq);
        if (shipResult.isErr()) {
            // Compensate: refund payment + release inventory
            String refundReq = "{\"refund\":" + chargeResult.getValue() + "}";
            h.cleatCall("payments", "Refund", refundReq);
            String releaseReq = "{\"release\":" + reservationJSON + "}";
            h.cleatCall("inventory", "Release", releaseReq);
            return "{\"error\":\"shipping failed: " + escapeJSON(shipResult.getError()) + "\"}";
        }

        // Step 4: Best-effort notification
        String notifyReq = "{\"notification\":" + input + "}";
        h.cleatCall("notifications", "SendEmail", notifyReq);

        return "{\"status\":\"shipped\"}";
    }

    /**
     * Cancel order workflow entry point.
     * <p>
     * Checks for a cancellation signal before proceeding with
     * cancellation logic.
     *
     * @param h     the {@link HostCalls} instance for durable orchestration
     * @param input the JSON-encoded order identifier
     * @return a JSON result string indicating cancellation status
     */
    @CleatEntry(name = "cancel_order")
    public static String cancelOrder(HostCalls h, String input) {
        // Check for cancellation signal
        CleatResult<Boolean> cancelCheck = h.pollCancellation();
        if (cancelCheck.isOk() && cancelCheck.getValue()) {
            return "{\"status\":\"cancelled\"}";
        }

        // Proceed with cancellation
        String cancelReq = "{\"cancel\":" + input + "}";
        CleatResult<String> result = h.cleatCall("orders", "Cancel", cancelReq);
        if (result.isErr()) {
            return "{\"error\":\"cancellation failed: " + escapeJSON(result.getError()) + "\"}";
        }

        return "{\"status\":\"cancelled\"}";
    }

    /**
     * Escape a string for safe embedding in a JSON string value.
     * <p>
     * Delegates to {@link cleat.JsonHelper#escapeJson} for TeaVM-safe
     * escaping (no regex or {@code String.format}).
     *
     * @param s the raw string
     * @return the JSON-escaped string, or {@code "null"} if the input is null
     */
    private static String escapeJSON(String s) {
        if (s == null) return "null";
        return cleat.JsonHelper.escapeJson(s);
    }
}
