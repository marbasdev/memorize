(() => {
  const th = localStorage.getItem('praxis_theme');
  if (th === 'light' || th === 'dark') document.documentElement.dataset.theme = th;
})();
