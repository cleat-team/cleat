// A typo in an EXTRA key MUST compile, and this file exists so that boundary is
// asserted rather than discovered.
//
// The emitted schema sets additionalProperties:true on purpose: it mirrors the
// binding, which never rejects an input object for carrying keys it does not
// look up (cleat#1690). openapi-typescript therefore emits an index signature.
// Loosening that to catch extra-key typos would make the document describe a
// server stricter than the one that exists.
import type { operations } from "./api";

type StartBody = operations["start_optionalparam"]["requestBody"]["content"]["application/json"];

export const extraKey: StartBody = { input: { userID: "u-1", nope: 1 } };
