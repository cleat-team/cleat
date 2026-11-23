"""Shared reading of changelog.d fragments (cleat's fragment scheme; see changelog.d/README.md). Imported by fold-changelog.py and
check-changelog-fragments.py so the two cannot disagree about what a well-formed fragment is."""
import pathlib
import re

DIR = pathlib.Path("changelog.d")
NAME_RE = re.compile(r"^(\d+)-([a-z0-9][a-z0-9-]*)\.md$")
# section key -> the heading CHANGELOG.md uses; the order is the order a release reads in.
SECTIONS = [
    ("upgrade", "### UPGRADE NOTES"),
    ("added", "### Added"),
    ("changed", "### Changed"),
    ("fixed", "### Fixed"),
    ("removed", "### Removed"),
]
KEYS = [k for k, _ in SECTIONS]


class Fragment:
    def __init__(self, path, issue, slug, section, body):
        self.path, self.issue, self.slug, self.section, self.body = path, issue, slug, section, body


def read(path):
    """Return (Fragment, None) or (None, why-not). Never raises on a malformed file: a guard that crashed on bad input would report nothing."""
    path = pathlib.Path(path)
    m = NAME_RE.match(path.name)
    if not m:
        return None, "%s: the name must be <issue>-<slug>.md (digits, a hyphen, a lowercase slug)" % path
    try:
        text = path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as e:
        return None, "%s: unreadable (%s)" % (path, e)
    lines = text.split("\n")
    if not lines or lines[0] != "---":
        return None, "%s: must start with a `---` front matter line holding `section: <name>`" % path
    try:
        end = lines.index("---", 1)
    except ValueError:
        return None, "%s: the front matter is never closed by a second `---`" % path
    fm = {}
    for line in lines[1:end]:
        if not line.strip():
            continue
        if ":" not in line:
            return None, "%s: front matter line %r is not `key: value`" % (path, line)
        k, v = line.split(":", 1)
        fm[k.strip()] = v.split("#", 1)[0].strip()
    section = fm.get("section", "")
    if section not in KEYS:
        return None, "%s: section %r is not one of %s" % (path, section, ", ".join(KEYS))
    extra = sorted(set(fm) - {"section"})
    if extra:
        return None, "%s: unknown front matter key(s) %s (only `section` is read)" % (path, ", ".join(extra))
    body = "\n".join(lines[end + 1:]).strip("\n")
    if not body.strip():
        return None, "%s: the body is empty" % path
    if not body.startswith("- "):
        return None, "%s: the body must start with `- ` (it is a changelog bullet, as it will read in CHANGELOG.md)" % path
    for i, line in enumerate(body.split("\n")[1:], 2):
        if line and not line.startswith("  "):
            return None, "%s: body line %d is not indented two spaces; a fragment is ONE entry (continuation lines are indented)" % (path, i)
    return Fragment(path, int(m.group(1)), m.group(2), section, body), None


def read_all(directory=None):
    d = pathlib.Path(directory) if directory else DIR
    frags, problems = [], []
    if not d.is_dir():
        return frags, problems
    for p in sorted(d.iterdir()):
        if p.name in ("README.md", ".gitkeep") or p.is_dir():
            continue
        f, why = read(p)
        if why:
            problems.append(why)
        else:
            frags.append(f)
    return frags, problems


def order(frags):
    """Newest issue first within a section, then by slug: the same on every machine."""
    return sorted(frags, key=lambda f: (-f.issue, f.slug))
