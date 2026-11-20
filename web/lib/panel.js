// Status panel logic. Styles live in tailwind/panel.css (compiled to /lib/panel.css).
const WINDOW = 300; // seconds of history the server keeps

const numberFull = new Intl.NumberFormat('en-US');
const numberCompact = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 });
const full = value => numberFull.format(value || 0);
const compact = value => (Math.abs(value || 0) < 10000 ? numberFull.format(Math.round((value || 0) * 10) / 10) : numberCompact.format(value));
const clock = time => new Date(time).toLocaleTimeString('en-GB', { hour12: false });
const plural = (count, one, many) => `${full(count)} ${count === 1 ? one : many}`;

// Axis ceiling: a round number whose half is round too
function niceMax(value) {
    if (value <= 4) return 4;

    const power = Math.pow(10, Math.floor(Math.log10(value)));
    const steps = power === 1 ? [4, 6, 8, 10] : [1, 2, 4, 6, 8, 10];

    return steps.map(step => step * power).find(candidate => candidate >= value);
}

// Single-series time chart: line, soft fill, minimal grid, crosshair with value and a table view
Vue.component('ts-chart', {
    props: { values: Array, end: Number, color: String, name: String, bucket: String, height: Number, table: Boolean },
    data: () => ({ width: 600, index: null }),
    template: `
        <div class="chart relative mt-auto pt-2.5" ref="box">
            <div v-if="table" class="scroll-thin overflow-y-auto" :style="{ height: height + 'px' }">
                <table class="w-full border-collapse">
                    <thead><tr><th class="th">Until</th><th class="th text-right">{{ bucket === 'sum' ? 'Messages in 10 s' : 'Connections' }}</th></tr></thead>
                    <tbody><tr v-for="row in rows" :key="row.at"><td class="td font-mono text-[12.5px]">{{ row.label }}</td><td class="td text-right tabular-nums">{{ row.value }}</td></tr></tbody>
                </table>
            </div>
            <template v-else>
                <svg :width="width" :height="height" role="img" tabindex="0" :aria-label="aria"
                    @pointermove="move" @pointerleave="index = null" @blur="index = null" @keydown="key">
                    <line v-for="tick in yTicks" :key="'g' + tick.value" :class="tick.value ? 'gridline' : 'baseline'" :x1="left" :x2="width - right" :y1="tick.y" :y2="tick.y"></line>
                    <text v-for="tick in yTicks" :key="'y' + tick.value" :x="left - 8" :y="tick.y + 3.5" text-anchor="end">{{ tick.label }}</text>
                    <text v-for="tick in xTicks" :key="'x' + tick.label" :x="tick.x" :y="height - 5" :text-anchor="tick.anchor">{{ tick.label }}</text>
                    <path v-if="points.length > 1" class="area" :d="area" :style="{ fill: color }"></path>
                    <path v-if="points.length > 1" class="line" :d="line" :style="{ stroke: color }"></path>
                    <line v-if="hover" class="cross" :x1="hover.x" :x2="hover.x" :y1="top" :y2="height - bottom"></line>
                    <circle v-if="mark" class="dot" :cx="mark.x" :cy="mark.y" r="4.5" :style="{ fill: color }"></circle>
                </svg>
                <div v-if="hover" class="pointer-events-none absolute top-3.5 z-20 grid grid-cols-[auto_auto] items-baseline gap-x-2 rounded-lg bg-ink px-2.5 py-1.5 whitespace-nowrap text-surface" :style="tipStyle">
                    <strong class="font-display text-[15px] font-bold">{{ hover.value }}</strong>
                    <span class="text-xs opacity-85"><i class="mr-1.5 inline-block h-0.5 w-3 rounded-full align-middle" :style="{ background: color }"></i>{{ name }}</span>
                    <small class="col-span-2 font-mono text-[11px] opacity-70">{{ hover.when }}</small>
                </div>
            </template>
        </div>`,
    computed: {
        left() { return 40; }, right() { return 12; }, top() { return 12; }, bottom() { return 24; },
        max() { return niceMax(Math.max(0, ...this.values)); },
        points() {
            const plotWidth = this.width - this.left - this.right;
            const plotHeight = this.height - this.top - this.bottom;
            const count = this.values.length;

            // The newest sample sits on the right edge; the axis always spans the whole window
            return this.values.map((value, i) => ({
                value,
                age: count - 1 - i,
                x: this.width - this.right - (count - 1 - i) / (WINDOW - 1) * plotWidth,
                y: this.top + plotHeight * (1 - value / this.max),
            }));
        },
        line() { return this.points.map((p, i) => (i ? 'L' : 'M') + p.x.toFixed(1) + ' ' + p.y.toFixed(1)).join(''); },
        area() {
            const base = this.height - this.bottom;
            const first = this.points[0], last = this.points[this.points.length - 1];

            return `${this.line}L${last.x.toFixed(1)} ${base}L${first.x.toFixed(1)} ${base}Z`;
        },
        yTicks() {
            const plotHeight = this.height - this.top - this.bottom;

            return [0, this.max / 2, this.max].map(value => ({ value, label: compact(value), y: this.top + plotHeight * (1 - value / this.max) }));
        },
        xTicks() {
            const plotWidth = this.width - this.left - this.right;
            const shown = this.width < 420 ? [5, 3, 1, 0] : [5, 4, 3, 2, 1, 0];

            return shown.map(minutes => ({
                label: minutes ? `−${minutes} min` : 'now',
                x: this.width - this.right - minutes * 60 / (WINDOW - 1) * plotWidth,
                anchor: minutes === 0 ? 'end' : (minutes === 5 ? 'start' : 'middle'),
            }));
        },
        hover() {
            const point = this.index === null ? null : this.points[this.index];

            if (!point) return null;

            return {
                x: point.x, y: point.y,
                value: full(point.value),
                when: (point.age ? `${point.age} s ago` : 'now') + ' · ' + clock(this.end - point.age * 1000),
            };
        },
        // Without a pointer, the dot marks the current value
        mark() { return this.hover || this.points[this.points.length - 1] || null; },
        tipStyle() {
            const flip = this.hover.x > this.width - 170;

            return { left: this.hover.x + 'px', transform: flip ? 'translateX(calc(-100% - 10px))' : 'translateX(10px)' };
        },
        // Table view: 10-second buckets, newest first
        rows() {
            const rows = [];

            for (let endIndex = this.values.length; endIndex > 0; endIndex -= 10) {
                const slice = this.values.slice(Math.max(0, endIndex - 10), endIndex);
                const at = this.end - (this.values.length - endIndex) * 1000;

                rows.push({ at, label: clock(at), value: full(this.bucket === 'sum' ? slice.reduce((a, b) => a + b, 0) : slice[slice.length - 1]) });
            }

            return rows;
        },
        aria() {
            const last = this.values[this.values.length - 1] || 0;

            return `Chart of ${this.name} over the last 5 minutes. Current value: ${full(last)}. Use the arrow keys to move through it.`;
        },
    },
    mounted() {
        this.observer = new ResizeObserver(entries => { this.width = Math.max(240, Math.floor(entries[0].contentRect.width)); });
        this.observer.observe(this.$refs.box);
    },
    beforeDestroy() { this.observer.disconnect(); },
    methods: {
        move(event) {
            if (!this.points.length) return;

            const x = event.clientX - this.$el.getBoundingClientRect().left;
            let nearest = 0;

            this.points.forEach((p, i) => { if (Math.abs(p.x - x) < Math.abs(this.points[nearest].x - x)) nearest = i; });
            this.index = nearest;
        },
        key(event) {
            if (!['ArrowLeft', 'ArrowRight', 'Home', 'End', 'Escape'].includes(event.key) || !this.points.length) return;

            event.preventDefault();

            const last = this.points.length - 1;
            const current = this.index === null ? last : this.index;

            this.index = { ArrowLeft: Math.max(0, current - 1), ArrowRight: Math.min(last, current + 1), Home: 0, End: last, Escape: null }[event.key];
        },
    },
});

const remember = {
    get(key) { try { return localStorage.getItem(key); } catch (error) { return null; } },
    set(key, value) { try { localStorage.setItem(key, value); } catch (error) {} },
};

new Vue({
    el: '#app',
    data: {
        online: false,
        lastSeen: 0,
        latency: 0,
        now: Date.now(),
        dark: false,
        appId: '',
        stats: {
            app: { id: '', name: '' }, apps: [], appKey: '', addr: '', uptimeSeconds: 0,
            connections: 0, peakConnections: 0, totalConnections: 0, published: 0, delivered: 0,
            rejectedSubscriptions: 0, rejectedRequests: 0, droppedSlow: 0,
            channels: [], sockets: [], recent: [],
            history: { end: null, connections: [], published: [], delivered: [] },
            runtime: { goVersion: '', goroutines: 0, memoryMB: 0 },
        },
        tables: { connections: false, delivered: false },
        channelSearch: '',
        channelType: 'all',
        channelTypes: [{ id: 'all', label: 'All' }, { id: 'public', label: 'Public' }, { id: 'private', label: 'Private' }, { id: 'presence', label: 'Presence' }],
        openChannel: null,
        socketSearch: '',
        feedSearch: '',
        feed: [],
        paused: false,
        pending: 0,
        freshAfter: Infinity,
        test: { channel: 'test', text: 'Hello from the panel', connected: false, subscribed: false, received: [] },
    },
    computed: {
        // Which application the panel is looking at; empty means the server's first one
        appQuery() { return this.appId ? `?app=${encodeURIComponent(this.appId)}` : ''; },
        historyEnd() { return this.stats.history.end ? new Date(this.stats.history.end).getTime() : Date.now(); },
        uptime() { return this.span(this.stats.uptimeSeconds); },
        offlineFor() { return this.span(Math.max(0, Math.round((this.now - this.lastSeen) / 1000))); },
        rateNow() {
            const last = this.stats.history.delivered.slice(-10);

            return last.length ? last.reduce((a, b) => a + b, 0) / last.length : 0;
        },
        publishedLastMinute() { return this.stats.history.published.slice(-60).reduce((a, b) => a + b, 0); },
        fanout() { return this.stats.published ? compact(this.stats.delivered / this.stats.published) : '0'; },
        rejections() { return this.stats.rejectedSubscriptions + this.stats.rejectedRequests + this.stats.droppedSlow; },
        rejectionDetail() {
            return [
                [this.stats.rejectedSubscriptions, 'channel signature', 'channel signatures'],
                [this.stats.rejectedRequests, 'API request', 'API requests'],
                [this.stats.droppedSlow, 'slow browser', 'slow browsers'],
            ].filter(([count]) => count).map(([count, one, many]) => plural(count, one, many)).join(' · ');
        },
        memory() { return `${this.stats.runtime.memoryMB < 10 ? this.stats.runtime.memoryMB.toFixed(1) : Math.round(this.stats.runtime.memoryMB)} MB`; },
        channelMix() {
            const count = type => this.stats.channels.filter(c => c.type === type).length;

            if (!this.stats.channels.length) return 'Nobody subscribed';

            return [[count('public'), 'public', 'public'], [count('private'), 'private', 'private'], [count('presence'), 'presence', 'presence']]
                .filter(([n]) => n).map(([n, one, many]) => plural(n, one, many)).join(' · ');
        },
        maxSubscribers() { return Math.max(1, ...this.stats.channels.map(c => c.subscribers)); },
        visibleChannels() {
            const term = this.channelSearch.toLowerCase();

            return this.stats.channels
                .filter(c => (this.channelType === 'all' || c.type === this.channelType) && c.name.toLowerCase().includes(term))
                .sort((a, b) => b.subscribers - a.subscribers || a.name.localeCompare(b.name));
        },
        visibleSockets() {
            const term = this.socketSearch.toLowerCase();

            return this.stats.sockets.filter(s => [s.id, s.remote, s.origin, s.agent, ...s.channels].join(' ').toLowerCase().includes(term));
        },
        visibleFeed() {
            const term = this.feedSearch.toLowerCase();

            return this.feed.filter(e => `${e.channel} ${e.event} ${e.source}`.toLowerCase().includes(term)).slice(0, 120);
        },
    },
    watch: {
        // Switching application starts from a clean view of the new one
        appId(value) {
            if (value === this.stats.app.id) return;

            Object.assign(this, { feed: [], pending: 0, freshAfter: Infinity, lastSeen: 0, openChannel: null });
            this.refresh();
        },
    },
    created() {
        const saved = remember.get('ws.theme');

        if (saved) document.documentElement.dataset.theme = saved;

        const system = matchMedia('(prefers-color-scheme: dark)');

        this.dark = saved ? saved === 'dark' : system.matches;
        // Until a theme is picked by hand, the button follows the system
        system.addEventListener('change', event => { if (!remember.get('ws.theme')) this.dark = event.matches; });
        this.refresh();
        setInterval(this.refresh, 1000);
    },
    methods: {
        full, compact, clock, plural,

        async refresh() {
            const started = performance.now();

            this.now = Date.now();

            try {
                const response = await fetch('/stats' + this.appQuery, { cache: 'no-store' });

                if (!response.ok) throw new Error(response.status);

                const stats = await response.json();

                this.latency = Math.max(1, Math.round(performance.now() - started));
                this.updateFeed(stats.recent);
                this.stats = stats;
                if (!this.appId) this.appId = stats.app.id;
                this.online = true;
                this.lastSeen = Date.now();
            } catch (error) {
                this.online = false;
            }
        },

        // While paused the list freezes and only counts what arrives
        updateFeed(recent) {
            const newest = this.feed.length ? this.feed[0].id : 0;
            const incoming = recent.filter(e => e.id > newest).length;

            if (this.paused) {
                this.pending = incoming;
                return;
            }

            if (!incoming) return;

            // The first load highlights nothing: only what arrives while the panel is open
            this.freshAfter = this.lastSeen ? newest : Infinity;
            this.feed = recent;
        },

        togglePause() {
            this.paused = !this.paused;

            if (!this.paused) {
                this.pending = 0;
                this.freshAfter = this.feed.length ? this.feed[0].id : 0;
                this.feed = this.stats.recent;
            }
        },

        toggleTheme() {
            this.dark = !this.dark;
            document.documentElement.dataset.theme = this.dark ? 'dark' : 'light';
            remember.set('ws.theme', this.dark ? 'dark' : 'light');
        },

        toggleChannel(name) { this.openChannel = this.openChannel === name ? null : name; },
        socketsIn(channel) { return this.stats.sockets.filter(s => s.channels.includes(channel)); },
        typeLabel(type) { return { public: 'Public', private: 'Private', presence: 'Presence' }[type]; },
        memberName(member) { return (member.info && (member.info.name || member.info.nick)) || member.id; },

        span(seconds) {
            if (seconds < 60) return `${seconds} s`;
            if (seconds < 3600) return `${Math.floor(seconds / 60)} min`;
            if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ${Math.floor(seconds % 3600 / 60)} min`;

            return `${Math.floor(seconds / 86400)} d ${Math.floor(seconds % 86400 / 3600)} h`;
        },
        ago(time) { return this.span(Math.max(0, Math.round((this.now - new Date(time).getTime()) / 1000))); },
        bytes(size) { return size < 1024 ? `${size} B` : `${(size / 1024).toFixed(1)} KB`; },

        // Short name of the browser or client, from its User-Agent
        client(agent) {
            if (!agent) return 'Unknown';

            const system = /Windows/.test(agent) ? 'Windows' : /Android/.test(agent) ? 'Android' : /iPhone|iPad/.test(agent) ? 'iOS' : /Mac OS X/.test(agent) ? 'macOS' : /Linux/.test(agent) ? 'Linux' : '';
            const browser = /Edg\//.test(agent) ? 'Edge' : /OPR\//.test(agent) ? 'Opera' : /Firefox\//.test(agent) ? 'Firefox' : /Chrome\//.test(agent) ? 'Chrome' : /Safari\//.test(agent) ? 'Safari' : '';

            if (browser) return system ? `${browser} · ${system}` : browser;

            return agent.length > 28 ? agent.slice(0, 28) + '…' : agent;
        },

        // Connects the way pusher-js does: handshake, channel subscription and pings
        connectTest() {
            const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
            const socket = new WebSocket(`${scheme}://${location.host}/app/${this.stats.appKey}?protocol=7&client=panel`);

            this.test.connected = true;

            socket.onmessage = frame => {
                const message = JSON.parse(frame.data);

                if (message.event === 'pusher:connection_established') {
                    socket.send(JSON.stringify({ event: 'pusher:subscribe', data: { channel: this.test.channel } }));
                } else if (message.event === 'pusher_internal:subscription_succeeded') {
                    this.test.subscribed = true;
                } else if (message.channel === this.test.channel) {
                    const data = JSON.parse(message.data);

                    this.test.received.unshift({
                        time: new Date(), event: message.event, text: data.text || message.data,
                        ms: data.sentAt ? (performance.timeOrigin + performance.now() - data.sentAt).toFixed(1) : '–',
                    });
                }
            };

            socket.onclose = () => { this.test.connected = false; this.test.subscribed = false; };

            setInterval(() => { if (socket.readyState === 1) socket.send(JSON.stringify({ event: 'pusher:ping', data: {} })); }, 25000);
        },

        sendTest() {
            if (!this.test.subscribed) return;

            fetch('/debug/send' + this.appQuery, {
                method: 'POST',
                body: JSON.stringify({
                    name: 'Test', channel: this.test.channel,
                    data: JSON.stringify({ text: this.test.text, sentAt: performance.timeOrigin + performance.now() }),
                }),
            });
        },
    },
});
