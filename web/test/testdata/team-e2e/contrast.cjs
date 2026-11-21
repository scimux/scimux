'use strict';

function rgb(value) {
  if (typeof value !== 'string') return null;
  const match = /^rgb\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)\s*\)$/.exec(value);
  if (!match) return null;
  const parts = match.slice(1).map(Number);
  if (parts.some(part => part > 255)) return null;
  return parts.map(part => { const v=part/255; return v<=0.04045?v/12.92:((v+0.055)/1.055)**2.4; });
}

function contrastSample(sample) {
  if (!sample || !sample.connected || !sample.visible) return null;
  const text=rgb(sample.text),background=rgb(sample.background);
  if (!text || !background) return null;
  const lum=c=>c[0]*0.2126+c[1]*0.7152+c[2]*0.0722;
  const a=lum(text),b=lum(background);
  return {...sample,contrast:(Math.max(a,b)+0.05)/(Math.min(a,b)+0.05)};
}

module.exports = {contrastSample};
