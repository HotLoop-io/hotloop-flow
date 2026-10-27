<script lang="ts">
  import '../tokens.css';
  import '../fonts.css';
  import '../theme.css';
  import '../canvas.css';
  import { onMount } from 'svelte';
  onMount(() => {
    void import('../main-legacy');
  });
</script>

<div id="legacy"></div>
