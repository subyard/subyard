(() => {
  'use strict';
  const slides = [...document.querySelectorAll('.slide')];
  const deck = document.getElementById('deck');
  const viewport = document.getElementById('viewport');
  const previous = document.getElementById('previous');
  const next = document.getElementById('next');
  const notes = document.getElementById('notes');
  let current = 0;
  function show(index, updateHash = true) {
    current = Math.max(0, Math.min(slides.length - 1, index));
    slides.forEach((slide, i) => {
      slide.classList.toggle('active', i === current);
      slide.setAttribute('aria-hidden', String(i !== current));
      slide.inert = i !== current;
    });
    document.getElementById('counter').textContent = `${current + 1} / ${slides.length}`;
    document.getElementById('slide-title').textContent = slides[current].dataset.title;
    document.getElementById('progress').style.width = `${100 * (current + 1) / slides.length}%`;
    previous.disabled = current === 0;
    next.disabled = current === slides.length - 1;
    notes.href = `notes.html#slide-${current + 1}`;
    if (updateHash) history.replaceState(null, '', `#${current + 1}`);
  }
  function resize() {
    deck.style.transform = `scale(${Math.min(viewport.clientWidth / 1600, viewport.clientHeight / 900)})`;
  }
  function fromHash() {
    const requested = Number(location.hash.slice(1));
    show(Number.isInteger(requested) && requested > 0 ? requested - 1 : 0, false);
  }
  async function fullscreen() {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else await document.documentElement.requestFullscreen();
    } catch { /* The browser may disallow fullscreen; the deck remains usable. */ }
  }
  previous.addEventListener('click', () => show(current - 1));
  next.addEventListener('click', () => show(current + 1));
  document.getElementById('fullscreen').addEventListener('click', fullscreen);
  window.addEventListener('keydown', event => {
    if (event.altKey || event.ctrlKey || event.metaKey || event.target.closest('button, a, input, textarea')) return;
    if (['ArrowRight', 'ArrowDown', 'PageDown', ' '].includes(event.key)) { event.preventDefault(); show(current + 1); }
    else if (['ArrowLeft', 'ArrowUp', 'PageUp'].includes(event.key)) { event.preventDefault(); show(current - 1); }
    else if (event.key === 'Home') { event.preventDefault(); show(0); }
    else if (event.key === 'End') { event.preventDefault(); show(slides.length - 1); }
    else if (event.key.toLowerCase() === 'f') fullscreen();
    else if (event.key.toLowerCase() === 'n') window.open(notes.href, '_blank', 'noopener');
  });
  window.addEventListener('hashchange', fromHash);
  window.addEventListener('resize', resize);
  window.addEventListener('beforeprint', () => slides.forEach(s => { s.inert = false; s.removeAttribute('aria-hidden'); }));
  window.addEventListener('afterprint', () => show(current, false));
  fromHash();
  resize();
})();
