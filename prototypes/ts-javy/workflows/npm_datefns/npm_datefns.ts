import { workflow } from "../../packages/cleat-ts/index";
import { format, addDays, addMonths, startOfWeek, endOfMonth, differenceInCalendarDays, eachDayOfInterval, isWeekend, parseISO, isValid, parse, formatDistance, formatDistanceToNow, subDays } from "date-fns";
import { de } from "date-fns/locale";
import { runCase, type Cases } from "../npm_common";

// Node is run with TZ=UTC; the guest has no time zone database and reads as UTC (PHASE2.md "Not determined"). Same wall clock either way.
const d = new Date(Date.UTC(2024, 1, 29, 13, 5, 9));

export const CASES: Cases = {
  format: () => format(d, "yyyy-MM-dd'T'HH:mm:ss EEEE, MMMM do, QQQ"),
  arithmetic: () => [addDays(d, 366), addMonths(d, 12), startOfWeek(d), endOfMonth(d)].map((x) => x.toISOString()).concat(String(differenceInCalendarDays(addDays(d, 40), d))),
  interval: () => eachDayOfInterval({ start: d, end: addDays(d, 6) }).map((x) => (isWeekend(x) ? "w" : "d")).join(""),
  parse: () => [parseISO("2024-02-29T13:05:09Z").getTime(), isValid(parseISO("nope")), parse("29/02/2024", "dd/MM/yyyy", d).toISOString()],
  distance: () => [formatDistance(d, addDays(d, -40), { addSuffix: true }), formatDistance(d, addDays(d, 400))],
  // Reads the clock through the Date shim: the answer ("3 days") does not depend on when it runs, but the computation does.
  distance_to_now: () => formatDistanceToNow(subDays(new Date(), 3)),
  locale_de: () => format(d, "PPPP", { locale: de }),
  // The root cause of `parse` above, with no package in the way: QuickJS's Date setters return NaN for a non-finite argument but leave the date
  // UNCHANGED (quickjs.c set_date_field: `if (!res) return JS_NAN;` before JS_SetThisTimeValue), where the spec makes it an Invalid Date.
  // date-fns builds dates with setUTCFullYear(year), so parseISO("nope") comes back as 1970-01-01 instead of Invalid Date.
  native_date_setter_nan: () => {
    const x = new Date(0);
    x.setUTCFullYear(NaN);
    return String(x);
  },
  control_differs: () => typeof (globalThis as any).process,
};

export const npmDatefns = workflow<{ case: string }, unknown>(async (ctx, input) => runCase(ctx, input.case, CASES));
