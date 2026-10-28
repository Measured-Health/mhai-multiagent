"""Every module in the package imports cleanly.

Guards against broken package exports and leftover imports from the
original notebooks, both of which once made intelligence.agents unimportable.
"""

import importlib
import pkgutil

import pytest

import intelligence

MODULES = sorted(
    m.name
    for m in pkgutil.walk_packages(intelligence.__path__, prefix="intelligence.")
    if m.name != "intelligence.__main__"
)


@pytest.mark.parametrize("name", MODULES)
def test_module_imports(name):
    importlib.import_module(name)
