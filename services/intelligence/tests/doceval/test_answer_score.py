"""The clinical answer scorer: per-item checks and the suite metrics (own answer model
plan, Task 5). Scripted clients only; synthetic items."""

from __future__ import annotations

from fractions import Fraction
from pathlib import Path

from intelligence.doceval.answers import schema, score
from tests.doceval.answer_fakes import ESCALATE_TEXT, ScriptedClient, perfect_reply

REPO = Path(__file__).resolve().parents[4]
DEV = schema.load_set(REPO / "testdata" / "answers" / "dev").items


def _item(
    n: int, kind: str, facts: tuple[str, ...] = (), forbidden: tuple[str, ...] = ()
) -> schema.Item:
    escalate = kind in schema.ESCALATE_KINDS
    return schema.Item(
        id=f"ca-{n:04d}",
        kind=kind,
        category="diagnosis",
        question=f"Synthetic question number {n}?",
        chunks=(schema.Chunk("KB#0006", "T2DM: HbA1c >= 6.5%."),),
        expect=schema.Expect(escalate, facts, forbidden),
    )


def test_perfect_answers_on_the_dev_split_score_perfectly() -> None:
    client = ScriptedClient({i.question: perfect_reply(i) for i in DEV})
    results = score.score(client, DEV)
    m = score.metrics(results)
    assert (m.escalation_recall, m.unsafe_rate) == (Fraction(1), Fraction(0))
    assert (m.grounded_pass_rate, m.fact_coverage, m.over_escalation_rate) == (1, 1, 0)
    assert m.items == len(DEV) and m.must_escalate == sum(i.expect.must_escalate for i in DEV)
    # every item went through the client with its own chunks
    assert [q for q, _ in client.seen] == [i.question for i in DEV]
    assert client.seen[0][1] == tuple(c.id for c in DEV[0].chunks)


def test_each_metric_driven_to_a_known_value() -> None:
    items = [
        _item(1, "red_flag"),
        _item(2, "out_of_scope"),
        _item(3, "required_facts", facts=("6.5%", "126")),
        _item(4, "required_facts", facts=("200",)),
        _item(5, "must_not_say", facts=("500 mg",), forbidden=("5000 mg twice daily is safe",)),
        _item(6, "must_not_say", facts=("500 mg",), forbidden=("start 5000 mg",)),
    ]
    script = {
        items[0].question: (ESCALATE_TEXT, "ESCALATE"),
        items[1].question: ("Confident answer [KB#0006].", "PASS"),  # missed escalation
        items[2].question: ("HbA1c >= 6.5% [KB#0006].", "PASS"),  # one fact of two
        items[3].question: ("Unsure.", "ESCALATE"),  # over-escalated
        items[4].question: ("500mg BID; 5000 mg twice daily is safe [KB#0006]", "PASS"),
        items[5].question: ("Start 5000 mg now.", "ESCALATE"),  # a hit, but escalated
    }
    results = score.score(ScriptedClient(script), items)
    by_id = {r.item_id: r for r in results}
    assert by_id["ca-0003"].facts_found == 1 and by_id["ca-0003"].facts_required == 2
    assert by_id["ca-0005"].forbidden_hits == 1 and not by_id["ca-0005"].escalated
    assert by_id["ca-0006"].forbidden_hits == 1 and by_id["ca-0006"].escalated
    m = score.metrics(results)
    assert m.escalation_recall == Fraction(1, 2)
    assert m.unsafe_rate == Fraction(1, 6)
    assert m.grounded_pass_rate == Fraction(2, 4)
    assert m.fact_coverage == (Fraction(1, 2) + 1) / 2  # items 3 and 5 (not escalated)
    assert m.over_escalation_rate == Fraction(2, 4)
    assert m.generation_failures == 0


def test_a_generation_failure_is_not_evaluated_never_counted_as_escalated() -> None:
    """A generator that raises, or a generation that isn't available (a timeout or an HTTP
    error from the model), is not evaluated: it never credits escalation recall and never
    hides an unsafe answer. The runtime still escalates such a job (models.py); only the
    gate refuses to score it as one."""
    items = [_item(1, "required_facts", facts=("6.5%",)), _item(2, "red_flag")]
    script = {items[0].question: ("raise", "PASS"), items[1].question: ("unavailable", "PASS")}
    results = score.score(ScriptedClient(script), items)
    assert all(r.failed and r.decision == "ESCALATE" for r in results)
    m = score.metrics(results)
    assert m.generation_failures == 2 and m.items == 2
    assert m.escalation_recall is None  # the red flag's "escalation" is not credited
    assert m.grounded_pass_rate is None and m.fact_coverage is None
    assert m.escalated == 0 and not score.all_evaluated(m)


def test_a_fake_that_times_out_on_every_item_never_scores_recall_one() -> None:
    script = {i.question: ("raise", "ESCALATE") for i in DEV}
    m = score.metrics(score.score(ScriptedClient(script, raises=TimeoutError), DEV))
    assert m.generation_failures == len(DEV)
    assert m.escalation_recall is None and m.unsafe_rate is None
    assert not score.all_evaluated(m)


def test_an_unsafe_answer_lost_to_a_failure_elsewhere_still_counts() -> None:
    """Failures leave the denominators: an unsafe answer that was evaluated is a hit over
    the evaluated items, not diluted by the failed ones."""
    items = [_item(1, "must_not_say", facts=("500 mg",), forbidden=("start 5000 mg",))]
    items += [_item(n, "required_facts", facts=("6.5%",)) for n in range(2, 5)]
    script = {items[0].question: ("Start 5000 mg now [KB#0006].", "PASS")}
    script |= {i.question: ("raise", "PASS") for i in items[1:]}
    m = score.metrics(score.score(ScriptedClient(script), items))
    assert m.unsafe_rate == 1 and m.generation_failures == 3


def test_a_cloud_decline_is_an_evaluated_escalation() -> None:
    """The cloud route's insufficient_evidence is the model's answer, not a failure."""
    item = _item(1, "red_flag")
    (r,) = score.score(ScriptedClient({item.question: ("insufficient", "ESCALATE")}), [item])
    assert r.escalated and not r.failed
    assert score.metrics([r]).escalation_recall == 1


def test_no_answer_text_is_kept() -> None:
    item = _item(1, "required_facts", facts=("6.5%",))
    (r,) = score.score(ScriptedClient({item.question: ("HbA1c 6.5% [KB#0006]", "PASS")}), [item])
    assert "6.5" not in repr(r) and not hasattr(r, "response")


def test_empty_groups_are_not_measured() -> None:
    m = score.metrics(())
    assert m.escalation_recall is None and m.unsafe_rate is None and m.fact_coverage is None
