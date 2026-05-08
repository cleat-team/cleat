// Travel booking — parallel flight/hotel/car booking with Saga.AddParallel.
//
// Demonstrates:
//   - Saga.AddParallel for concurrent booking with automatic LIFO compensation
//   - PollCancellation for mid-booking cancellation
//   - DurableCallTyped for type-safe service calls
//   - SetQueryState for external status queries
//
// Build:
//
//	cleat build -o /tmp/out ./examples/travel/
package travel

import (
	"encoding/json"
	"fmt"

	"github.com/rcownie/cleat/durable"
)

var h cleat.HostCalls

// ---- Domain types ----

type BookingInput struct {
	UserID string        `json:"user_id"`
	Flight FlightRequest `json:"flight"`
	Hotel  HotelRequest  `json:"hotel"`
	Car    CarRequest    `json:"car"`
}

type FlightRequest struct {
	Origin      string `json:"origin"`
	Destination string `json:"destination"`
	Date        string `json:"date"`
	Passengers  int    `json:"passengers"`
}

type HotelRequest struct {
	City     string `json:"city"`
	CheckIn  string `json:"check_in"`
	CheckOut string `json:"check_out"`
	Guests   int    `json:"guests"`
}

type CarRequest struct {
	City        string `json:"city"`
	PickupDate  string `json:"pickup_date"`
	DropoffDate string `json:"dropoff_date"`
}

type BookingResult struct {
	BookingID string `json:"booking_id"`
	FlightRef string `json:"flight_ref,omitempty"`
	HotelRef  string `json:"hotel_ref,omitempty"`
	CarRef    string `json:"car_ref,omitempty"`
	TotalUSD  int    `json:"total_usd"`
	Status    string `json:"status"`
}

// ---- Entry point ----

func BookTravel(h cleat.HostCalls, input BookingInput) (*BookingResult, error) {
	if err := validateInput(input); err != nil {
		return nil, err
	}

	bookingID := fmt.Sprintf("TRIP-%s-%d", input.UserID, h.Now().UnixMilli())
	h.SetQueryState("booking_id", bookingID)
	h.SetQueryState("status", "booking")

	if cancelled, reason := h.PollCancellation(); cancelled {
		return canceled(bookingID, reason), nil
	}

	// ---- Parallel booking with Saga.AddParallel ----
	// All three bookings run concurrently. If any fail, successfully
	// completed steps are automatically compensated in LIFO order.

	var flightRef, hotelRef, carRef string
	var totalUSD int

	parallelSteps := []cleat.SagaStep{
		{
			Forward: func(h cleat.HostCalls) (string, error) {
				resp, err := h.DurableCall("flights", "Book", toJSON(input.Flight))
				if err != nil {
					return "", fmt.Errorf("flight booking failed: %w", err)
				}
				var r struct {
					Ref      string `json:"ref"`
					PriceUSD int    `json:"price_usd"`
				}
				json.Unmarshal([]byte(resp), &r)
				flightRef = r.Ref
				return r.Ref, nil
			},
			Compensate: func(h cleat.HostCalls) error {
				if flightRef == "" {
					return nil
				}
				h.DurableLog(fmt.Sprintf("Compensating flight: %s", flightRef))
				h.DurableCall("flights", "Cancel", toJSON(map[string]string{"ref": flightRef}))
				return nil
			},
		},
		{
			Forward: func(h cleat.HostCalls) (string, error) {
				resp, err := h.DurableCall("hotels", "Book", toJSON(input.Hotel))
				if err != nil {
					return "", fmt.Errorf("hotel booking failed: %w", err)
				}
				var r struct {
					Ref      string `json:"ref"`
					PriceUSD int    `json:"price_usd"`
				}
				json.Unmarshal([]byte(resp), &r)
				hotelRef = r.Ref
				return r.Ref, nil
			},
			Compensate: func(h cleat.HostCalls) error {
				if hotelRef == "" {
					return nil
				}
				h.DurableLog(fmt.Sprintf("Compensating hotel: %s", hotelRef))
				h.DurableCall("hotels", "Cancel", toJSON(map[string]string{"ref": hotelRef}))
				return nil
			},
		},
	}

	if input.Car.City != "" {
		parallelSteps = append(parallelSteps, cleat.SagaStep{
			Forward: func(h cleat.HostCalls) (string, error) {
				resp, err := h.DurableCall("cars", "Book", toJSON(input.Car))
				if err != nil {
					return "", fmt.Errorf("car booking failed: %w", err)
				}
				var r struct {
					Ref      string `json:"ref"`
					PriceUSD int    `json:"price_usd"`
				}
				json.Unmarshal([]byte(resp), &r)
				carRef = r.Ref
				return r.Ref, nil
			},
			Compensate: func(h cleat.HostCalls) error {
				if carRef == "" {
					return nil
				}
				h.DurableLog(fmt.Sprintf("Compensating car: %s", carRef))
				h.DurableCall("cars", "Cancel", toJSON(map[string]string{"ref": carRef}))
				return nil
			},
		})
	}

	s := cleat.NewSaga()
	s.AddParallel(parallelSteps...)

	if err := s.Run(h); err != nil {
		h.SetQueryState("status", "failed")
		return nil, fmt.Errorf("booking failed (all compensated): %w", err)
	}

	// Check for cancellation after booking.
	if cancelled, reason := h.PollCancellation(); cancelled {
		cancelAll(h, flightRef, hotelRef, carRef)
		return canceled(bookingID, reason), nil
	}

	// ---- Confirmation ----
	h.DurableCall("notifications", "SendConfirmation", toJSON(map[string]interface{}{
		"booking_id": bookingID,
		"user_id":    input.UserID,
		"flight_ref": flightRef,
		"hotel_ref":  hotelRef,
		"car_ref":    carRef,
		"total_usd":  totalUSD,
	}))

	h.SetQueryState("status", "confirmed")
	h.DurableLog(fmt.Sprintf("Travel booked: %s (flight=%s, hotel=%s, car=%s)",
		bookingID, flightRef, hotelRef, carRef))

	return &BookingResult{
		BookingID: bookingID,
		FlightRef: flightRef,
		HotelRef:  hotelRef,
		CarRef:    carRef,
		Status:    "confirmed",
	}, nil
}

// ---- Helpers ----

func validateInput(input BookingInput) error {
	if input.UserID == "" {
		return fmt.Errorf("user_id is required")
	}
	if input.Flight.Origin == "" || input.Flight.Destination == "" {
		return fmt.Errorf("flight origin and destination are required")
	}
	return nil
}

func cancelAll(h cleat.HostCalls, flightRef, hotelRef, carRef string) {
	if flightRef != "" {
		h.DurableCall("flights", "Cancel", toJSON(map[string]string{"ref": flightRef}))
	}
	if hotelRef != "" {
		h.DurableCall("hotels", "Cancel", toJSON(map[string]string{"ref": hotelRef}))
	}
	if carRef != "" {
		h.DurableCall("cars", "Cancel", toJSON(map[string]string{"ref": carRef}))
	}
}

func canceled(bookingID, reason string) *BookingResult {
	return &BookingResult{
		BookingID: bookingID,
		Status:    fmt.Sprintf("canceled: %s", reason),
	}
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
