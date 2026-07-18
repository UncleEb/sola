"use strict";

// Background starfield: a gentle warp-speed flight through space, drawn on a
// full-window canvas behind all content. Purely decorative — it never blocks
// interaction (pointer-events: none in CSS) and honours reduced-motion.
(function () {
    const canvas = document.getElementById("starfield");
    if (!canvas) {
        return;
    }
    const ctx = canvas.getContext("2d");

    const STAR_COUNT = 320;
    const SPEED = 2; // depth units travelled toward the viewer per frame
    const FOCAL = 128; // perspective strength: larger spreads stars out faster
    const COLOR = "180, 210, 255"; // faint blue-white points of light

    let w, h, cx, cy, stars;

    // A star lives in 3D: x/y offset from centre and depth z. z runs from w
    // (far, near the vanishing point) down to 0 (past the viewer). atFront
    // scatters z across the whole range for the initial fill; respawns start
    // far away so they stream outward from the centre.
    function makeStar(atFront) {
        return {
            x: (Math.random() - 0.5) * w,
            y: (Math.random() - 0.5) * h,
            z: atFront ? Math.random() * w : w,
        };
    }

    function resize() {
        const dpr = window.devicePixelRatio || 1;
        w = window.innerWidth;
        h = window.innerHeight;
        cx = w / 2;
        cy = h / 2;
        canvas.width = w * dpr;
        canvas.height = h * dpr;
        canvas.style.width = w + "px";
        canvas.style.height = h + "px";
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
        stars = Array.from({ length: STAR_COUNT }, () => makeStar(true));
    }

    // project maps a star's 3D position to screen coordinates. Closer stars
    // (smaller z) fan out further from centre and appear larger/brighter.
    function project(s, z) {
        const k = FOCAL / z;
        return { x: s.x * k + cx, y: s.y * k + cy };
    }

    function step() {
        ctx.clearRect(0, 0, w, h);

        for (const s of stars) {
            const prev = project(s, s.z + SPEED);
            s.z -= SPEED;

            if (s.z < 1) {
                Object.assign(s, makeStar(false));
                continue;
            }

            const p = project(s, s.z);
            if (p.x < 0 || p.x > w || p.y < 0 || p.y > h) {
                Object.assign(s, makeStar(false));
                continue;
            }

            const depth = 1 - s.z / w; // 0 far … 1 near
            ctx.strokeStyle = `rgba(${COLOR}, ${Math.min(1, depth * 1.2)})`;
            ctx.lineWidth = depth * 2.2 + 0.2;
            ctx.lineCap = "round";
            ctx.beginPath();
            // A short streak from the previous position sells the motion; far
            // stars barely move and read as points, near ones as light streaks.
            ctx.moveTo(prev.x, prev.y);
            ctx.lineTo(p.x, p.y);
            ctx.stroke();
        }

        requestAnimationFrame(step);
    }

    // Reduced-motion: render a single static field of dots instead of animating.
    function drawStatic() {
        ctx.clearRect(0, 0, w, h);
        for (const s of stars) {
            const p = project(s, s.z);
            const depth = 1 - s.z / w;
            ctx.fillStyle = `rgba(${COLOR}, ${Math.min(1, depth * 1.2)})`;
            ctx.beginPath();
            ctx.arc(p.x, p.y, depth * 1.6 + 0.3, 0, Math.PI * 2);
            ctx.fill();
        }
    }

    window.addEventListener("resize", resize);
    resize();

    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
        drawStatic();
    } else {
        requestAnimationFrame(step);
    }
})();
