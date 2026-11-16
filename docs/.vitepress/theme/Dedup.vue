<script setup lang="ts">
const UNIT = 4
const GAP = 4
const PAD = 10
const sizes = { a: 18, b: 26, c: 14, d: 22, e: 20, c2: 17, f: 12 }
const hash = { a: '3fa9', b: 'c41e', c: '07bd', d: 'e2f0', e: '91a6', c2: '5d38', f: 'b7c2' }
type Id = keyof typeof sizes

const row = (ids: Id[], y: number, h: number) => {
  let x = PAD
  return ids.map((id) => {
    const w = sizes[id] * UNIT
    const box = { id, x, y, w, h, cx: x + w / 2 }
    x += w + GAP
    return box
  })
}

const first = row(['a', 'b', 'c', 'd', 'e'], 30, 34)
const store = row(['a', 'b', 'c', 'd', 'e', 'c2', 'f'], 152, 44)
const second = row(['a', 'b', 'c2', 'd', 'e', 'f'], 284, 34)
const slot = Object.fromEntries(store.map((s) => [s.id, s]))
const isNew = (id: Id) => id === 'c2' || id === 'f'

const link = (cx: number, from: number, to: number, id: Id) => {
  const mid = (from + to) / 2
  return `M${cx} ${from}C${cx} ${mid} ${slot[id].cx} ${mid} ${slot[id].cx} ${to}`
}
</script>

<template>
  <figure class="dedup">
    <svg viewBox="0 0 560 350" role="img" aria-labelledby="dedup-title dedup-desc">
      <title id="dedup-title">How ark deduplicates two snapshots</title>
      <desc id="dedup-desc">
        The first snapshot is split into five chunks, all stored. The second snapshot has six chunks; four already
        exist in the repository and only two new ones are added. Eleven chunks are referenced, seven are stored.
      </desc>

      <text class="label a-label" :x="PAD" y="20">snapshot 01</text>
      <text class="label note a-label" x="426" y="20" text-anchor="end">5 chunks · 5 new</text>
      <g v-for="(c, i) in first" :key="c.id" class="chunk a-chunk" :style="`--i:${i}`">
        <rect :x="c.x" :y="c.y" :width="c.w" :height="c.h" rx="3" />
        <text :x="c.cx" :y="c.y + c.h / 2">{{ hash[c.id] }}</text>
      </g>
      <path
        v-for="(c, i) in first"
        :key="c.id"
        class="link a-link"
        pathLength="1"
        :style="`--i:${i}`"
        :d="link(c.cx, 64, 152, c.id)"
      />

      <text class="label s-label" x="550" y="136" text-anchor="end">repository</text>
      <rect class="vault s-label" x="3" y="144" width="554" height="60" rx="7" />
      <g
        v-for="(c, i) in store"
        :key="c.id"
        :class="['chunk stored', isNew(c.id) ? 's-new' : 's-old']"
        :style="`--i:${i}`"
      >
        <rect :x="c.x" :y="c.y" :width="c.w" :height="c.h" rx="3" />
        <text :x="c.cx" :y="c.y + c.h / 2">{{ hash[c.id] }}</text>
      </g>

      <path
        v-for="(c, i) in second"
        :key="c.id"
        :class="['link', isNew(c.id) ? 'b-new' : 'b-reuse']"
        pathLength="1"
        :style="`--i:${i}`"
        :d="link(c.cx, 284, 196, c.id)"
      />
      <g v-for="(c, i) in second" :key="c.id" :class="['chunk b-chunk', { fresh: isNew(c.id) }]" :style="`--i:${i}`">
        <rect :x="c.x" :y="c.y" :width="c.w" :height="c.h" rx="3" />
        <text :x="c.cx" :y="c.y + c.h / 2">{{ hash[c.id] }}</text>
      </g>
      <text class="label b-label" :x="PAD" y="338">snapshot 02</text>
      <text class="label note b-label" x="490" y="338" text-anchor="end">6 chunks · 2 new</text>
    </svg>
    <figcaption>
      <span><b>11</b> chunks referenced</span>
      <span><b>7</b> stored</span>
    </figcaption>
  </figure>
</template>

<style scoped>
.dedup {
  margin: 0;
}
svg {
  display: block;
  width: 100%;
  height: auto;
  overflow: visible;
  font-family: var(--vp-font-family-mono);
}
text {
  font-size: 10px;
  fill: var(--ark-muted);
}
.label {
  font-size: 9.5px;
  letter-spacing: 0.12em;
  text-transform: uppercase;
}
.note {
  fill: var(--ark-faint);
}
.chunk rect {
  fill: var(--ark-panel);
  stroke: var(--ark-line);
}
.chunk text {
  text-anchor: middle;
  dominant-baseline: central;
}
.fresh rect {
  stroke: var(--ark-cyan);
}
.fresh text {
  fill: var(--ark-cyan);
}
.vault {
  fill: color-mix(in srgb, var(--ark-blue) 6%, transparent);
  stroke: var(--ark-line);
}
.stored rect {
  fill: color-mix(in srgb, var(--ark-blue) 26%, var(--vp-c-bg));
  stroke: color-mix(in srgb, var(--ark-blue) 70%, var(--vp-c-bg));
}
.stored text {
  fill: var(--ark-code);
}
.s-new rect {
  fill: color-mix(in srgb, var(--ark-cyan) 24%, var(--vp-c-bg));
  stroke: var(--ark-cyan);
}
.link {
  fill: none;
  stroke: var(--ark-blue);
  stroke-width: 1.25;
  stroke-dasharray: 1;
}
.b-new {
  stroke: var(--ark-cyan);
}
.b-reuse {
  stroke: var(--ark-muted);
  stroke-dasharray: 0.02 0.03;
}

figcaption {
  display: flex;
  gap: 1.75rem;
  margin-top: 1rem;
  padding-inline-start: 0.6rem;
  font-family: var(--vp-font-family-mono);
  font-size: 0.75rem;
  letter-spacing: 0.04em;
  color: var(--ark-muted);
}
figcaption b {
  font-family: var(--ark-font-display);
  font-size: 1.5rem;
  font-weight: 600;
  color: var(--ark-ink);
  margin-inline-end: 0.35rem;
}
figcaption span:last-child b {
  color: var(--ark-cyan);
}

@media (prefers-reduced-motion: no-preference) {
  .a-label,
  .a-chunk,
  .s-label,
  .s-old,
  .b-label,
  .b-chunk,
  .b-reuse,
  .s-new,
  figcaption {
    animation: rise 0.5s both cubic-bezier(0.2, 0.7, 0.2, 1);
    animation-delay: calc(var(--t) + var(--i, 0) * 70ms);
  }
  .a-link,
  .b-new {
    animation: draw 0.7s both ease-out;
    animation-delay: calc(var(--t) + var(--i, 0) * 70ms);
  }
  .a-label,
  .a-chunk {
    --t: 0.2s;
  }
  .a-link {
    --t: 0.7s;
  }
  .s-label {
    --t: 0.9s;
  }
  .s-old {
    --t: 1.2s;
  }
  .b-label,
  .b-chunk {
    --t: 2s;
  }
  .b-reuse {
    --t: 2.5s;
    animation-name: fade;
  }
  .b-new {
    --t: 2.9s;
  }
  .s-new {
    --t: 3.3s;
  }
  figcaption {
    --t: 3.7s;
  }
}
@keyframes rise {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
}
@keyframes fade {
  from {
    opacity: 0;
  }
}
@keyframes draw {
  from {
    stroke-dashoffset: 1;
  }
}
</style>
