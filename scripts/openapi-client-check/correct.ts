// The call a well-typed client writes. It MUST compile.
//
// This is the positive control: a generated client that refuses everything is
// useless, and "the misspelled call failed" is satisfied by a client with no
// usable types at all.
import type { operations } from "./api";

type StartBody = operations["start_optionalparam"]["requestBody"]["content"]["application/json"];

export const minimal: StartBody = { input: { userID: "u-1" } };

// The pointer parameter is genuinely optional: omitted here, present and
// non-null below, and accepted as null, which is what a pointer binds.
export const withPromo: StartBody = {
  input: { userID: "u-1", promo: { code: "SAVE10", off_cents: 1000 } },
};
export const withNullPromo: StartBody = { input: { userID: "u-1", promo: null } };

// The entry point is an enum, so naming it is allowed.
export const named: StartBody = { input: { userID: "u-1" }, entry_point: "apply_coupon" };
