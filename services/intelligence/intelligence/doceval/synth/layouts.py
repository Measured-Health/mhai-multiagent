"""Layout specs for synthetic documents: data plus a seed, no drawing code.

``for_type(doc_type, template_seed)`` returns :data:`LAYOUTS_PER_TYPE` layouts
that differ from each other in font, size, columns, table style, header block,
logo, stamp and page flow. The same arguments always give the same layouts.

Template seeds are split into two disjoint ranges so a layout the model trains
on is never one it is evaluated on: :data:`TRAIN_TEMPLATE_SEEDS` and
:data:`EVAL_TEMPLATE_SEEDS` (dev and test). A build for ``purpose="train"``
refuses an eval seed, and the dev and test purposes refuse a train seed.

Separation is by seed only. A layout built from an eval seed can still look like one
built from a train seed (the same fonts, header styles and table styles recur), so
this does not measure how a model copes with a template family it has never seen.
Held-out layout families (unseen templates) come with set v1.1.

Logos are drawn shapes and the organisation names are invented from the word
lists below; nothing here is, or imitates, a real logo.
"""

import random
from dataclasses import dataclass
from typing import Literal

TRAIN_TEMPLATE_SEEDS = range(0, 1_000_000)
EVAL_TEMPLATE_SEEDS = range(1_000_000, 1_100_000)

DOC_TYPES = (
    "lab_report",
    "clinic_note",
    "discharge_summary",
    "medication_chart",
    "vitals_chart",
)
FONT_FAMILIES = ("dejavu-sans", "dejavu-serif", "dejavu-mono", "liberation-sans")
TABLE_STYLES = ("ruled", "zebra", "borderless")
HEADER_STYLES = ("logo-left", "centred", "logo-right", "band", "plain")
LOGO_SHAPES = ("circle", "square", "triangle", "hexagon", "bars")
STAMPS: tuple[str | None, ...] = (None, "RECEIVED", "COPY", "VERIFIED", "FILED")
PAGE_SIZES = ("a4", "letter")
LAYOUTS_PER_TYPE = 16
BODY_PT = (8.0, 9.0, 10.0, 11.0, 12.0)

# Dark, non-red accents: the tests find a field by its pure-red ink, so no
# furniture may be reddish.
ACCENTS = (
    (20, 60, 110),
    (10, 90, 80),
    (60, 50, 110),
    (35, 35, 45),
    (0, 80, 120),
    (50, 90, 40),
    (80, 55, 100),
)
_ORG_FIRST = (
    "Alder",
    "Brightwater",
    "Cedarbrook",
    "Dunmore",
    "Elmshade",
    "Fernlea",
    "Gannet",
    "Hollin",
    "Ivywell",
    "Juniper",
    "Kestrel",
    "Larchmont",
)
_ORG_LAST = (
    "Health Centre",
    "Medical Group",
    "Diagnostics",
    "Family Clinic",
    "Hospital (demo)",
    "Care Partners",
)

Purpose = Literal["train", "dev", "test"]


class LayoutSeedError(ValueError):
    """A template seed is out of range or is in the wrong split for the purpose."""


@dataclass(frozen=True)
class LayoutSpec:
    doc_type: str
    template_seed: int
    index: int
    font: str
    body_pt: float
    columns: int
    table_style: str
    header_style: str
    logo_shape: str
    org_name: str
    accent: tuple[int, int, int]
    stamp: str | None
    page_size: str
    margin_mm: int
    line_spacing: float
    label_style: int
    new_page_per_section: bool

    def signature(self) -> tuple[object, ...]:
        """What makes two layouts look different (everything but the seed and index)."""
        return (
            self.font,
            self.body_pt,
            self.columns,
            self.table_style,
            self.header_style,
            self.logo_shape,
            self.org_name,
            self.accent,
            self.stamp,
            self.page_size,
            self.margin_mm,
            self.line_spacing,
            self.label_style,
            self.new_page_per_section,
        )


def check_seed(template_seed: int, purpose: str) -> None:
    """Refuse a seed outside both ranges, or in the wrong split for ``purpose``.

    ``purpose`` must be exactly ``train``, ``dev`` or ``test``; anything else is refused.
    """
    if isinstance(template_seed, bool) or not isinstance(template_seed, int):
        raise LayoutSeedError("template seed must be an integer")
    in_train = template_seed in TRAIN_TEMPLATE_SEEDS
    in_eval = template_seed in EVAL_TEMPLATE_SEEDS
    if not (in_train or in_eval):
        raise LayoutSeedError("template seed outside the train and eval ranges")
    match purpose:
        case "train":
            if in_eval:
                raise LayoutSeedError("an eval template seed cannot be used to build training data")
        case "dev" | "test":
            if in_train:
                raise LayoutSeedError(
                    "a train template seed cannot be used to build dev or test data"
                )
        case _:
            raise LayoutSeedError("purpose must be train, dev or test")


def for_type(doc_type: str, template_seed: int, *, purpose: Purpose) -> list[LayoutSpec]:
    """The layouts of ``doc_type`` for a template seed, all distinct, in a fixed order.

    ``purpose`` is required (no default): the split check can never be skipped.
    """
    if doc_type not in DOC_TYPES:
        raise ValueError("unknown document type")
    check_seed(template_seed, purpose)
    base = random.Random(f"layouts/{doc_type}/{template_seed}")
    # Offsets make the combinations differ between seeds while, for a fixed seed,
    # (font, header style) is unique across the 16 layouts: 4 and 5 are coprime.
    off_font, off_header, off_table = base.randrange(4), base.randrange(5), base.randrange(3)
    out = []
    for i in range(LAYOUTS_PER_TYPE):
        rng = random.Random(f"layout/{doc_type}/{template_seed}/{i}")
        out.append(
            LayoutSpec(
                doc_type=doc_type,
                template_seed=template_seed,
                index=i,
                font=FONT_FAMILIES[(i + off_font) % 4],
                body_pt=rng.choice(BODY_PT),
                columns=2 if rng.random() < 0.4 else 1,
                table_style=TABLE_STYLES[(i // 4 + off_table) % 3],
                header_style=HEADER_STYLES[(i + off_header) % 5],
                logo_shape=rng.choice(LOGO_SHAPES),
                org_name=f"{rng.choice(_ORG_FIRST)} {rng.choice(_ORG_LAST)}",
                accent=rng.choice(ACCENTS),
                stamp=rng.choice(STAMPS),
                page_size=rng.choice(PAGE_SIZES),
                margin_mm=rng.choice((12, 15, 18, 22)),
                line_spacing=rng.choice((1.2, 1.3, 1.45)),
                label_style=rng.randrange(3),
                new_page_per_section=(i % 4 == 3),
            )
        )
    return out
