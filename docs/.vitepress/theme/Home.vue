<script setup lang="ts">
import { computed } from 'vue'
import { useData } from 'vitepress'
import { VPButton } from 'vitepress/theme-without-fonts'
import Dedup from './Dedup.vue'

const { frontmatter } = useData()
const landing = computed(() => frontmatter.value.landing)
</script>

<template>
  <div v-if="landing" class="landing">
    <section class="hero">
      <div class="copy">
        <p class="eyebrow">
          <template v-for="(word, i) in landing.eyebrow" :key="word"><i v-if="i" />{{ word }}</template>
        </p>
        <h1>{{ landing.title }}</h1>
        <p class="tagline">{{ landing.tagline }}</p>
        <div class="actions">
          <VPButton
            v-for="(action, i) in landing.actions"
            :key="action.link"
            :theme="i ? 'alt' : 'brand'"
            :text="action.text"
            :href="action.link"
          />
        </div>
        <pre class="install"><span>$</span> {{ landing.install }}</pre>
      </div>
      <Dedup />
    </section>
    <ol class="pipeline">
      <li v-for="step in landing.steps" :key="step.title">
        <h2>{{ step.title }}</h2>
        <p>{{ step.details }}</p>
      </li>
    </ol>
  </div>
</template>

<style scoped>
.landing {
  margin: 0 auto;
  box-sizing: content-box;
  max-width: 1152px;
  padding: 0 24px;
  overflow-x: clip;
}
.hero {
  position: relative;
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: 3rem;
  align-items: center;
  padding-block: clamp(2rem, 7vmin, 5.5rem) 2rem;
}
.hero::before {
  content: '';
  position: absolute;
  inset: -6rem -50vw 0;
  z-index: -1;
  background-image:
    linear-gradient(var(--ark-panel) 1px, transparent 1px),
    linear-gradient(90deg, var(--ark-panel) 1px, transparent 1px);
  background-size: 3rem 3rem;
  mask-image: radial-gradient(ellipse 45% 70% at 62% 35%, #000 10%, transparent 75%);
}
.eyebrow {
  display: flex;
  align-items: center;
  gap: 0.7rem;
  font-family: var(--vp-font-family-mono);
  font-size: 0.75rem;
  letter-spacing: 0.12em;
  text-transform: uppercase;
  color: var(--ark-muted);
}
.eyebrow i {
  width: 4px;
  height: 4px;
  background: var(--ark-cyan);
}
h1 {
  margin-top: 1.25rem;
  font-family: var(--ark-font-display);
  font-size: clamp(2rem, 1rem + 3.4vw, 3.5rem);
  font-weight: 600;
  line-height: 1.08;
  letter-spacing: -0.05em;
  word-spacing: -0.2em;
  color: var(--ark-ink);
  text-wrap: balance;
}
.tagline {
  margin-top: 1.5rem;
  max-width: 34rem;
  font-size: 1.125rem;
  line-height: 1.6;
  color: var(--vp-c-text-1);
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 0.75rem;
  margin-top: 2rem;
}
.install {
  margin-top: 2rem;
  padding: 0.7rem 1rem;
  width: fit-content;
  max-width: 100%;
  overflow-x: auto;
  border: 1px solid var(--ark-line);
  border-radius: 6px;
  background: color-mix(in srgb, var(--ark-panel) 70%, transparent);
  font-family: var(--vp-font-family-mono);
  font-size: 0.875rem;
  color: var(--ark-code);
  user-select: all;
}
.install span {
  color: var(--ark-cyan);
  user-select: none;
}

.pipeline {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(13rem, 1fr));
  margin: 3rem 0 2.5rem;
  counter-reset: step;
  border-top: 1px solid var(--ark-line);
}
.pipeline li {
  counter-increment: step;
  padding: 1.5rem 1.5rem 1.75rem 0;
}
.pipeline li::before {
  content: counter(step, decimal-leading-zero);
  display: block;
  margin-top: calc(-1.5rem - 1px);
  padding-top: 1.25rem;
  width: 2rem;
  border-top: 1px solid var(--ark-cyan);
  font-family: var(--vp-font-family-mono);
  font-size: 0.75rem;
  letter-spacing: 0.1em;
  color: var(--ark-cyan);
}
.pipeline h2 {
  margin-top: 0.6rem;
  font-family: var(--ark-font-display);
  font-size: 1.25rem;
  font-weight: 600;
  letter-spacing: -0.025em;
  word-spacing: -0.2em;
  color: var(--ark-ink);
}
.pipeline p {
  margin-top: 0.5rem;
  font-size: 0.875rem;
  line-height: 1.6;
  color: var(--ark-muted);
}

@media (min-width: 640px) {
  .landing {
    padding: 0 48px;
  }
}
@media (min-width: 960px) {
  .landing {
    padding: 0 64px;
  }
}
@media (min-width: 72rem) {
  .hero {
    grid-template-columns: minmax(0, 5fr) minmax(0, 6fr);
    gap: 4rem;
  }
}
@media (prefers-reduced-motion: no-preference) {
  .copy > * {
    animation: in 0.6s both cubic-bezier(0.2, 0.7, 0.2, 1);
  }
  .copy > :nth-child(2) {
    animation-delay: 0.08s;
  }
  .copy > :nth-child(3) {
    animation-delay: 0.16s;
  }
  .copy > :nth-child(4) {
    animation-delay: 0.24s;
  }
  .copy > :nth-child(5) {
    animation-delay: 0.32s;
  }
}
@keyframes in {
  from {
    opacity: 0;
    transform: translateY(10px);
  }
}
</style>
