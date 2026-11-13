import { workflow } from "../../packages/cleat-ts/index";
import { z } from "zod";
import { runCase, type Cases } from "../npm_common";

const User = z.object({
  id: z.number().int().positive(),
  name: z.string().min(1).max(40),
  email: z.email(),
  role: z.enum(["admin", "user"]).default("user"),
  tags: z.array(z.string()).max(3).optional(),
});

export const CASES: Cases = {
  parse_ok: () => User.parse({ id: 7, name: "Ada", email: "ada@example.com" }),
  safeparse_errors: () => {
    const r = User.safeParse({ id: -1, name: "", email: "nope", role: "root", tags: ["a", "b", "c", "d"] });
    return r.success ? "unexpected success" : r.error.issues.map((i) => ({ path: i.path.join("."), code: i.code, message: i.message }));
  },
  transform_coerce: () =>
    z
      .object({
        n: z.coerce.number(),
        s: z.string().trim().toLowerCase().transform((s) => s.split(" ")),
        d: z.string().refine((s) => /^\d{4}-\d{2}-\d{2}$/.test(s), "bad date"),
      })
      .parse({ n: "42", s: "  Hello World ", d: "2024-02-29" }),
  discriminated_union: () => {
    const U = z.discriminatedUnion("kind", [z.object({ kind: z.literal("a"), x: z.number() }), z.object({ kind: z.literal("b"), y: z.string() })]);
    const bad = U.safeParse({ kind: "c" });
    return [U.parse({ kind: "a", x: 1 }), U.parse({ kind: "b", y: "z" }), bad.success ? "unexpected" : bad.error.issues[0].code];
  },
  json_schema: () => z.toJSONSchema(User),
  string_formats: () => ({
    uuid: z.uuid().safeParse("123e4567-e89b-42d3-a456-426614174000").success,
    url: z.url().safeParse("https://example.com/a?b=1").success,
    ipv4: z.ipv4().safeParse("10.0.0.256").success,
    iso: z.iso.datetime().safeParse("2024-02-29T13:05:09Z").success,
  }),
  // The root cause of `string_formats`.url: zod's url() validator does `new URL(...)` inside a try/catch, and this runtime has no URL, so the
  // ReferenceError is swallowed and a valid URL is reported invalid. Silent, not a throw.
  url_global: () => [typeof (globalThis as any).URL, typeof (globalThis as any).URLSearchParams],
  control_differs: () => typeof (globalThis as any).process, // the comparator's known-positive: Node says "object", the guest "undefined"
};

export const npmZod = workflow<{ case: string }, unknown>(async (ctx, input) => runCase(ctx, input.case, CASES));
