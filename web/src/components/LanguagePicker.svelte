<script lang="ts">
  // The language picker. Each language is written in itself, so a person who
  // opened the editor in a language they do not read can still find theirs.
  import { LANGUAGES, lang, setLanguage, tr } from '../lib/i18n.svelte';

  let { compact = false }: { compact?: boolean } = $props();
</script>

<select
  class="lang-picker"
  class:compact
  data-test="language-picker"
  aria-label={tr('lang.label')}
  title={tr('lang.label')}
  value={lang()}
  onchange={(e) => void setLanguage((e.currentTarget as HTMLSelectElement).value)}
>
  {#each LANGUAGES as l (l.code)}
    <option value={l.code} lang={l.code}>{l.name}</option>
  {/each}
</select>
