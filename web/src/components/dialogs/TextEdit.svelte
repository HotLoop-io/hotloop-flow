<script lang="ts">
  // A box of text with Cancel and a confirming button: a drafted test to
  // adjust before it is added, or the whole test suite as YAML. Resolves with
  // the text, or null when the person backs out.
  import Dialog from './Dialog.svelte';

  interface Props {
    title: string;
    intro?: string;
    text: string;
    rows: number;
    textTest: string;
    cancelTest: string;
    okLabel: string;
    okTest: string;
    done: (text: string | null) => void;
  }
  let { title, intro = '', text, rows, textTest, cancelTest, okLabel, okTest, done }: Props = $props();

  let value = $state(text);
  let area: HTMLTextAreaElement | undefined = $state();
  $effect(() => area?.focus());
</script>

<Dialog {title} size="wide" onclose={() => done(null)}>
  {#if intro}<p>{intro}</p>{/if}
  <textarea class="code test-yaml" spellcheck="false" {rows} data-test={textTest} bind:this={area} bind:value></textarea>
  {#snippet foot()}
    <div class="spacer"></div>
    <button class="ghost" data-test={cancelTest} onclick={() => done(null)}>Cancel</button>
    <button class="primary" data-test={okTest} onclick={() => done(value)}>{okLabel}</button>
  {/snippet}
</Dialog>
