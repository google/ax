# Repository rulesets

GitHub [rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets)
enforce how changes reach `main`. They are kept in this repository, in
[`.github/rulesets/`](../.github/rulesets), so a change to them is proposed,
reviewed and recorded like any other change.

## What is enforced

| Ruleset | Applies to | Rules |
|---|---|---|
| [`protect-main`](../.github/rulesets/protect-main.json) | `main` (`~DEFAULT_BRANCH`) | No deletion or force-push. Changes go through a pull request with 1 approving review, merged by squash or rebase. The `build-and-test` check must pass. |

The JSON files are the source of truth. If this table and a file disagree, the
file wins.

## Changing a ruleset

1. Edit or add a file in `.github/rulesets/`. Each file holds one ruleset, in
   the format of the [rulesets REST API](https://docs.github.com/en/rest/repos/rules),
   with only the policy fields: `name`, `target`, `enforcement`, `conditions`,
   `rules` and `bypass_actors`. The file name is the ruleset name, for example
   `protect-main.json`.
2. Open a pull request. The **Rulesets** workflow runs
   `hack/apply-rulesets.sh --dry-run` and prints how the change differs from
   what is live.
3. After the pull request is merged, a repo admin applies it:

   ```bash
   hack/apply-rulesets.sh --dry-run   # review the differences
   hack/apply-rulesets.sh             # create or update the live rulesets
   ```

   Rulesets are matched by name. The script never deletes a live ruleset; it
   lists live rulesets that have no file, and an admin deletes them in
   **Settings → Rules → Rulesets** once they are no longer needed.

New or stricter rulesets should start with `"enforcement": "evaluate"`. Once
the rule insights (**Settings → Rules → Insights**) show that they wouldn't
block legitimate work, a follow-up pull request switches them to `"active"`.

## Drift

The **Rulesets** workflow also runs `hack/apply-rulesets.sh --check` every day.
It fails when a live ruleset differs from its file, when a file hasn't been
applied, or when a live ruleset has no file. The fix is either to apply the
files, or, if the live change was intended, to export it with
`hack/apply-rulesets.sh --export` and open a pull request.

Only admins can read a ruleset's `bypass_actors`. For everyone else, including
the workflow, GitHub hides them, so they are left out of the comparison.
