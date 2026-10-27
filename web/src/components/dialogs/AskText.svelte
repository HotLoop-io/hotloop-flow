<script lang="ts">
  // One line of text. Resolves with it, or null if the person backed out or left it blank.
  import Dialog from './Dialog.svelte';

  interface Props {
    title: string;
    label: string;
    initial: string;
    test: string;
    done: (answer: string | null) => void;
  }
  let { title, label, initial, test, done }: Props = $props();

  let value = $state(initial);
  let input: HTMLInputElement | undefined = $state();
  $effect(() => {
    input?.focus();
    input?.select();
  });

  function ok(): void {
    const v = value.trim();
    if (v === '') return;
    done(v);
  }
</script>

<Dialog {title} size="narrow" backdropClose onclose={() => done(null)}>
  <div class="field">
    <label>{label}</label>
    <input type="text" bind:this={input} bind:value data-test="{test}-input" onkeydown={(e) => { if (e.key === 'Enter') ok(); }} />
  </div>
  {#snippet foot()}
    <button class="ghost" type="button" data-test="{test}-cancel" onclick={() => done(null)}>Cancel</button>
    <button class="primary" type="button" data-test="{test}-ok" onclick={ok}>Create</button>
  {/snippet}
</Dialog>
