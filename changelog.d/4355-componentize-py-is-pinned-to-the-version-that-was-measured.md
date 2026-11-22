---
section: changed
---
- **The Python install instructions now pin `componentize-py` exactly, to 0.25.1, and CI installs that same version from one hash-locked file (cleat#4355).** `componentize-py` decides which standard-library modules a Python component contains (cleat#4266), and the Python SDK README's compatibility tables were measured on 0.25.1 and on no other version, while the docs said `componentize-py>=0.12.0` and most CI jobs installed whatever pip resolved. Install it with `pip install cleat-sdk componentize-py==0.25.1`. A newer version is not known to be broken; it is not measured. `scripts/check-componentize-py-pin.py` (Lint) fails if anything installs it unpinned, puts a floor on it, or names another version.
