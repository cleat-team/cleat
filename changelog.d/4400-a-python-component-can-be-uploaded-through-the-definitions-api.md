---
section: fixed
---
- **A Python workflow component can now be uploaded through `POST /api/definitions` (cleat#4400).** The endpoint refused any body over a fixed 10 MiB, and a built Python component is about 19.5 MB (about 26 MB as the base64 the upload carries), so the API answered `413` for every Python workflow. The ceiling is now 64 MiB by default and has its own flag, `--max-definition-body-size`, which the `413` names; `--max-body-size` still does not move it. `cleat deploy` and `cleatctl deploy` write the database directly and were never affected. Memory: the worker holds the whole request while it decodes it, about 3 to 4 times the body at peak in one measurement, so a worker that takes several concurrent uploads should size the flag with that in mind.
