# A workflow the Python checker has nothing to say about.
#
# Its job in the suite is to be the arm that can only pass when something
# DECLINES to refuse. A gate that is not wired refuses nothing and satisfies
# the py004 arm's opposite perfectly, so a test with only the refusing arm
# passes against a tree with no gate at all.
from cleat_sdk import cleat_entry


@cleat_entry
def workflow(h, request):
    result = h.call("billing", "charge", {"amount": 100})
    return {"charged": True, "result": result}
