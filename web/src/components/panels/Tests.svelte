<script lang="ts">
  import { serverText, tr } from '../../lib/i18n.svelte';
  // The flow tests: how many there are, a run of them against what is on the
  // canvas, and what each one found. A problem about a node selects it.
  import type { TestReport } from '../../lib/api';

  export type TestsView =
    | { kind: 'info'; note: string; text: string }
    | { kind: 'running' }
    | { kind: 'report'; report: TestReport };

  interface Props {
    view: TestsView;
    onrun: () => void;
    onedit: () => void;
    onpick: (nodeId: string) => void;
  }
  let { view, onrun, onedit, onpick }: Props = $props();
</script>

<section class="section">
  <div class="section-head">
    <h3>{tr('panels.tests.tests')}</h3>
    <div class="spacer"></div>
    <button class="ghost sm" data-test="tests-run" title={tr('panels.tests.run_the_flow_tests_against')} onclick={onrun}>{tr('panels.tests.run')}</button>
    <button class="ghost sm" data-test="tests-edit" title={tr('panels.tests.edit_the_flow_tests')} onclick={onedit}>{tr('panels.tests.edit')}</button>
  </div>
  <div class="section-body">
    <div class="tests" data-test="tests">
      {#if view.kind === 'running'}
        <div class="empty">{tr('panels.tests.running')}</div>
      {:else if view.kind === 'info'}
        {#if view.note}<div class="test-summary pass" data-test="tests-note">{view.note}</div>{/if}
        <div class="empty" data-test="tests-info">{view.text}</div>
      {:else}
        {@const tests = view.report.tests ?? []}
        {@const ok = view.report.failed === 0 && view.report.errored === 0}
        <div class="test-summary {ok ? 'pass' : 'fail'}" data-test="tests-summary">
          {tests.length === 0 ? tr('panels.tests.no_tests_ran') : tr('panels.tests.summary', { passed: view.report.passed, failed: view.report.failed, errored: view.report.errored })}
        </div>
        {#each tests as t, i (i)}
          <div class="test-result {t.status}" data-test="test-result" data-status={t.status}>
            <div class="meta">
              <span class="kind test-{t.status}">{tr('panels.tests.status_' + t.status)}</span>
              <span class="name">{t.name}</span>
            </div>
            {#each t.problems ?? [] as p, j (j)}
              <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_noninteractive_element_interactions -->
              <pre class="diff-text test-problem" class:pickable={!!p.node} title={p.node ? tr('panels.tests.show_the_node') : undefined} onclick={() => { if (p.node) onpick(p.node); }}>{(p.expect ? tr('panels.tests.expect_n', { n: p.expect }) : '') + serverText(p.message)}</pre>
            {/each}
          </div>
        {/each}
      {/if}
    </div>
  </div>
</section>
