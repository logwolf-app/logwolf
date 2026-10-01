import { defineConfig } from 'vitepress';

export default defineConfig({
	title: 'Logwolf',
	description: 'Self-hosted event logging and observability.',

	head: [['link', { rel: 'icon', href: '/favicon.ico' }]],

	themeConfig: {
		nav: [
			{ text: 'Guide', link: '/getting-started' },
			{ text: 'SDK', link: '/sdk/js' },
			{ text: 'API', link: '/api' },
			{ text: 'GitHub', link: 'https://github.com/logwolf-app/logwolf' },
		],

		sidebar: [
			{
				text: 'Introduction',
				items: [
					{ text: 'Getting started', link: '/getting-started' },
					{ text: 'Self-hosting', link: '/self-hosting' },
					{ text: 'Architecture', link: '/architecture' },
				],
			},
			{
				text: 'SDK',
				items: [{ text: 'JavaScript', link: '/sdk/js' }],
			},
			{
				text: 'Reference',
				items: [{ text: 'HTTP API', link: '/api' }],
			},
		],

		socialLinks: [{ icon: 'github', link: 'https://github.com/logwolf-app/logwolf' }],

		footer: {
			message: 'Released under the GNU GPL v3 License.',
			copyright: 'Copyright © 2026 jpricardo',
		},

		search: {
			provider: 'local',
		},
	},
});
