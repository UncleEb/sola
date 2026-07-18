"use strict";

// Full-window background canvas with selectable modes: "none" (plain black),
// "starfield" (a gentle flight through space), and "warpspeed" (a hyperspace
// tunnel of curved cyan/magenta streaks over a black->flow radial glow). The
// mode comes from /api/settings; settings.js switches it live via
// window.applyBackground(mode).
(function () {
    const canvas = document.getElementById("background");
    if (!canvas) {
        return;
    }
    const ctx = canvas.getContext("2d");
    const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    let w, h, cx, cy;
    let mode = "starfield";
    let rafId = null;

    function stop() {
        if (rafId !== null) {
            cancelAnimationFrame(rafId);
            rafId = null;
        }
    }

    // ---- Starfield -------------------------------------------------------
    const STAR_COUNT = 320;
    const STAR_SPEED = 0.5; // depth units per frame
    const FOCAL = 128;
    const STAR_COLOR = "180, 210, 255";
    let stars = [];

    function makeStar(atFront) {
        return {
            x: (Math.random() - 0.5) * w,
            y: (Math.random() - 0.5) * h,
            z: atFront ? Math.random() * w : w,
        };
    }

    function projectStar(s, z) {
        const k = FOCAL / z;
        return { x: s.x * k + cx, y: s.y * k + cy };
    }

    function drawStarfield() {
        ctx.clearRect(0, 0, w, h);
        for (const s of stars) {
            const prev = projectStar(s, s.z + STAR_SPEED);
            s.z -= STAR_SPEED;
            if (s.z < 1) {
                Object.assign(s, makeStar(false));
                continue;
            }
            const p = projectStar(s, s.z);
            if (p.x < 0 || p.x > w || p.y < 0 || p.y > h) {
                Object.assign(s, makeStar(false));
                continue;
            }
            const depth = 1 - s.z / w;
            ctx.strokeStyle = `rgba(${STAR_COLOR}, ${Math.min(1, depth * 1.2)})`;
            ctx.lineWidth = depth * 2.2 + 0.2;
            ctx.lineCap = "round";
            ctx.beginPath();
            ctx.moveTo(prev.x, prev.y);
            ctx.lineTo(p.x, p.y);
            ctx.stroke();
        }
        rafId = requestAnimationFrame(drawStarfield);
    }

    function drawStarfieldStatic() {
        ctx.clearRect(0, 0, w, h);
        for (const s of stars) {
            const p = projectStar(s, s.z);
            const depth = 1 - s.z / w;
            ctx.fillStyle = `rgba(${STAR_COLOR}, ${Math.min(1, depth * 1.2)})`;
            ctx.beginPath();
            ctx.arc(p.x, p.y, depth * 1.6 + 0.3, 0, Math.PI * 2);
            ctx.fill();
        }
    }

    // ---- Warpspeed -------------------------------------------------------
    // A hyperspace tunnel: streaks spawn near a dark core and accelerate
    // OUTWARD to the edges. The acceleration (each streak moves faster the
    // farther out it is) is the perspective of flying forward through space.
    // Each streak has its own rate and a slight individual revolve, so the
    // field reads as an irregular tunnel rather than a coherent spinning spiral.
    const WARP_COUNT = 420;
    const WARP_ACCEL = 0.015; // fractional outward growth per frame (the "forward" feel)
    const WARP_BASE = 0.6; // base outward creep so streaks leave the core
    const WARP_INNER = 100; // spawn radius — the dark core
    const WARP_CYAN = "76, 194, 255"; // --flow
    const WARP_MAGENTA = "230, 90, 255";
    let warpParticles = [];
    let warpMaxR = 0;

    // (Re)spawn a streak near the core with a fresh random angle, rate, gentle
    // revolve direction and colour. initial spreads them across the radius for
    // the first frame so the tunnel starts full.
    function warpSpawn(p, initial) {
        p.angle = Math.random() * Math.PI * 2;
        p.spin = (Math.random() - 0.5) * 0.03; // gentle radial-dominant revolve (not a pinwheel)
        p.rate = 0.75 + Math.random() * 0.8; // per-streak speed variation
        p.color = Math.random() < 0.5 ? WARP_CYAN : WARP_MAGENTA;
        p.r = initial ? Math.random() * warpMaxR : WARP_INNER + Math.random() * 10;
        return p;
    }

    function seedWarp() {
        warpMaxR = Math.hypot(cx, cy) * 1.05;
        warpParticles = Array.from({ length: WARP_COUNT }, () => warpSpawn({}, true));
    }

    // warpStreakPath strokes a streak as a curved polyline that follows the
    // particle's real trajectory. Because radius grows geometrically while the
    // angle grows linearly, the path back toward the core is a logarithmic
    // spiral: at radius ρ the particle was at angle − k·ln(r/ρ), where
    // k = spin / ln(1 + accel). So the line itself curves — more spin, more bend.
    function warpStreakPath(p) {
        const t = Math.min(1, p.r / warpMaxR);
        const streakLen = 40 + t * 150;
        const rBack = Math.max(WARP_INNER, p.r - streakLen);
        const k = p.spin / Math.log(1 + WARP_ACCEL * p.rate);
        const segments = 12;

        ctx.beginPath();
        for (let i = 0; i <= segments; i++) {
            const rho = rBack + (p.r - rBack) * (i / segments);
            const ang = p.angle - k * Math.log(p.r / rho);
            const x = cx + Math.cos(ang) * rho;
            const y = cy + Math.sin(ang) * rho;
            i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
        }

        const alpha = Math.min(1, p.r / (WARP_INNER * 10)) * (1 - t * 0.1);
        ctx.strokeStyle = `rgba(${p.color}, ${alpha})`;
        ctx.lineWidth = 20 + t * 5.5; // thick, thickening toward the edge
        ctx.stroke();
    }

    function warpStreak(p) {
        // Exponential outward acceleration + a base creep near the centre.
        p.r = p.r * (1 + WARP_ACCEL * p.rate) + WARP_BASE * p.rate;
        p.angle += p.spin;

        warpStreakPath(p);

        if (p.r > warpMaxR) {
            warpSpawn(p, false);
        }
    }

    function drawWarp() {
        // Fade the previous frame by ERASING alpha (destination-out) rather than
        // painting black, so faded trails reveal the radial glow behind the
        // canvas instead of building up to opaque black.
        ctx.globalCompositeOperation = "destination-out";
        ctx.fillStyle = "rgba(0, 0, 0, 0.4)"; // only the alpha matters here
        ctx.fillRect(0, 0, w, h);
        ctx.globalCompositeOperation = "source-over";

        ctx.lineCap = "round";
        for (const p of warpParticles) {
            warpStreak(p);
        }
        rafId = requestAnimationFrame(drawWarp);
    }

    function drawWarpStatic() {
        ctx.clearRect(0, 0, w, h);
        ctx.lineCap = "round";
        for (const p of warpParticles) {
            warpStreakPath(p);
        }
    }

    // ---- Shared ----------------------------------------------------------
    function seed() {
        if (mode === "starfield") {
            stars = Array.from({ length: STAR_COUNT }, () => makeStar(true));
        } else if (mode === "warpspeed") {
            seedWarp();
        }
    }

    function render() {
        stop();
        ctx.clearRect(0, 0, w, h);
        if (mode === "starfield") {
            reduceMotion ? drawStarfieldStatic() : (rafId = requestAnimationFrame(drawStarfield));
        } else if (mode === "warpspeed") {
            reduceMotion ? drawWarpStatic() : (rafId = requestAnimationFrame(drawWarp));
        }
        // "none": leave the canvas cleared → the black page background shows.
    }

    function resize() {
        const dpr = window.devicePixelRatio || 1;
        w = window.innerWidth;
        h = window.innerHeight;
        cx = w / 2;
        cy = h / 2;
        canvas.width = Math.round(w * dpr);
        canvas.height = Math.round(h * dpr);
        canvas.style.width = w + "px";
        canvas.style.height = h + "px";
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        seed();
    }

    // Switch modes live (used by the settings page and the initial load).
    window.applyBackground = function (m) {
        mode = m === "none" || m === "warpspeed" ? m : "starfield";
        // Warpspeed gets a full-background blur (see style.css); other modes stay crisp.
        canvas.classList.toggle("background--blur", mode === "warpspeed");
        // The radial black->flow glow sits behind every animated mode, but not
        // "none" (which stays plain black).
        canvas.classList.toggle("background--glow", mode !== "none");
        // Expose the mode on <body> so the theme can adapt (e.g. more opaque
        // glass panels over the busy Warpspeed background).
        document.body.dataset.background = mode;
        seed();
        render();
    };

    window.addEventListener("resize", () => {
        resize();
        render();
    });

    resize(); // size the canvas before the first paint

    // Load the configured mode; fall back to the default if the fetch fails.
    fetch("/api/settings", { cache: "no-store" })
        .then((r) => (r.ok ? r.json() : Promise.reject(new Error(r.status))))
        .then((d) => window.applyBackground(d.background))
        .catch(() => window.applyBackground("starfield"));
})();
