// THE NEGATIVE CONTROL. It MUST NOT compile.
//
// "userId" for "userID" is a misspelled field, and because it is the REQUIRED
// one the input no longer satisfies the type. A client generated from the
// document is only worth generating if this fails with the field named.
//
// It is deliberately not a misspelled EXTRA key: additionalProperties:true
// means those compile, which extra-key.ts asserts.
import type { operations } from "./api";

type StartBody = operations["start_optionalparam"]["requestBody"]["content"]["application/json"];

export const misspelled: StartBody = { input: { userId: "u-1" } };
