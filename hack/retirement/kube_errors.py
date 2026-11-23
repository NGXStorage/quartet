#!/usr/bin/env python3
"""kubectl/oc error classification (error classification fix).

is_notfound must match ONLY the API NotFound reason. A UID-precondition
Conflict body reads "the server object was not found (uid precondition
failed)" — the words "not found" must NOT classify it as NotFound, or a
replacement object could be treated as gone and the run would continue.
"""


def is_notfound(rc, stderr):
    return rc != 0 and "(NotFound)" in stderr


def is_conflict(rc, stderr):
    return rc != 0 and "(Conflict)" in stderr


def is_forbidden(rc, stderr):
    return rc != 0 and "(Forbidden)" in stderr
