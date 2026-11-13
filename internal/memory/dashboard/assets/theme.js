(() => {
  const th = localStorage.getItem('memorize_theme');
  if (th === 'light' || th === 'dark') document.documentElement.dataset.theme = th;
})();
