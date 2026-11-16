// Cloudflare Pages "advanced mode" Worker.
// Redirect www.cubepod.dev and cubepod-docs.pages.dev to canonical https://cubepod.dev with a single 301 hop.
export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (
      url.hostname === 'www.cubepod.dev' ||
      url.hostname === 'cubepod-docs.pages.dev'
    ) {
      url.hostname = 'cubepod.dev';
      url.protocol = 'https:';
      url.port = '';
      return Response.redirect(url.toString(), 301);
    }
    return env.ASSETS.fetch(request);
  },
};
