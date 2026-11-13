import { workflow } from "../sdk";
export const w = workflow(async () => {
  setTimeout(() => {}, 10);
  return 1;
});
