// FOUC guard. Runs before the stylesheet is applied, so a dark-mode user never
// sees a white flash. It sets data-theme, which is what the tokens flip on.
(function () {
  try {
    var stored = localStorage.getItem('theme');
    var dark = stored ? stored === 'dark'
                      : window.matchMedia('(prefers-color-scheme: dark)').matches;
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  } catch (e) { /* private mode; fall through to light */ }
})();
