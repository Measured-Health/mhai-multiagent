"""Every module in the package imports cleanly.

Guards against broken package exports and leftover imports from the
original notebooks, both of which once made mhai.agents unimportable.
"""

import importlib
import pkgutil

import pytest

import mhai

MODULES = sorted(
    m.name
    for m in pkgutil.walk_packages(mhai.__path__, prefix="mhai.")
    if m.name != "mhai.__main__"
)


@pytest.mark.parametrize("name", MODULES)
def test_module_imports(name):
    importlib.import_module(name)
