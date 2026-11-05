#!/usr/bin/env python3
"""Copies branch_exact_test.go without the tests of what the fix added, for the replay of a pre-fix
commit: those tests call functions that the commit does not have, and would keep the package of the
decoy tests from compiling."""
import re
import sys

# The tests of helpers that the fix introduced; the decoy tests of the RPCs stay.
FIX_INTERNALS = ("TestResolveStartBranch", "TestSplitRevisionSuffix")

source = open(sys.argv[1], encoding="utf-8").read()
for name in FIX_INTERNALS:
    source = re.sub(r"(?:^//[^\n]*\n)*^func " + name + r"\(.*?^}\n", "", source, flags=re.M | re.S)
open(sys.argv[2], "w", encoding="utf-8").write(source)
