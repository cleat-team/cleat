"""Saga workflow - multi-step transaction with compensation."""

import json
from dataclasses import dataclass

from cleat_sdk import HostCalls, cleat_entry


@dataclass
class BookingRequest:
    user_id: str
    flight_id: str
    hotel_id: str
    amount_cents: int


@dataclass
class BookingResult:
    flight_reservation_id: str
    hotel_reservation_id: str
    payment_charge_id: str
    status: str


@cleat_entry
def book_travel(h: HostCalls, request: BookingRequest) -> BookingResult:
    """Book a flight + hotel + payment as a saga."""
    h.log(f"Starting travel booking for user {request.user_id}")
    result = BookingResult("", "", "", "pending")

    # Step 1: Reserve flight
    try:
        flight_resp = h.call(
            "flights", "Reserve", {"flight_id": request.flight_id, "user_id": request.user_id}
        )
        flight_data = json.loads(flight_resp)
        result.flight_reservation_id = flight_data["reservation_id"]
        h.log(f"Flight reserved: {result.flight_reservation_id}")
    except Exception as e:
        h.log(f"Flight reservation failed: {e}")
        result.status = f"failed: {e}"
        return result

    # Step 2: Reserve hotel
    try:
        hotel_resp = h.call(
            "hotels", "Reserve", {"hotel_id": request.hotel_id, "user_id": request.user_id}
        )
        hotel_data = json.loads(hotel_resp)
        result.hotel_reservation_id = hotel_data["reservation_id"]
        h.log(f"Hotel reserved: {result.hotel_reservation_id}")
    except Exception as e:
        # Compensate: cancel flight
        h.log(f"Hotel reservation failed: {e}. Compensating flight...")
        h.call("flights", "Cancel", {"reservation_id": result.flight_reservation_id})
        result.status = f"failed: {e}"
        return result

    # Step 3: Process payment
    try:
        payment_resp = h.call(
            "payments", "Charge", {"user_id": request.user_id, "amount_cents": request.amount_cents}
        )
        payment_data = json.loads(payment_resp)
        result.payment_charge_id = payment_data["charge_id"]
        h.log(f"Payment processed: {result.payment_charge_id}")
    except Exception as e:
        # Compensate: cancel flight + hotel
        h.log(f"Payment failed: {e}. Compensating all...")
        h.call("flights", "Cancel", {"reservation_id": result.flight_reservation_id})
        h.call("hotels", "Cancel", {"reservation_id": result.hotel_reservation_id})
        result.status = f"failed: {e}"
        return result

    result.status = "confirmed"
    h.log(f"Travel booking complete for user {request.user_id}")
    return result
