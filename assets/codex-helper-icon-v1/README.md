# Codex 小助手图标：抱闪电的小机器人

日期：2026-10-27（Asia/Singapore）。使用内置 ImageGen 生成；没有使用 API／CLI fallback。

设计：薄荷绿圆润机器人，深蓝面罩与弯弯的笑眼，抱着金色闪电，米白圆角底板。闪电呼应预热与任务接力；无文字，让 Dock 小尺寸仍能识别。

![图标预览](codex-helper-icon.png)

- `codex-helper-icon.png`：选定的生成原图，1254 × 1254，带透明通道。
- `CodexHelper.iconset/`：使用系统 sips 缩放的标准 macOS 尺寸及 Retina 版本。
- `CodexHelper.icns`：使用系统 iconutil 打包的 macOS 图标文件。

保留项目原有的 assets/icon.png 与 assets/icon.svg；本轮交付新设计与图标文件，尚未改动构建脚本、安装应用或登录项。透明外缘保留生成结果，不宣称已经完成像素级轮廓清理；作为最终安装图标前可再做定稿。

## 最终选用图的生成提示词

```text
Use case: stylized-concept / logo-brand. Asset type: a single polished macOS application icon for a personal desktop helper called Codex 小助手, with no lettering. Primary request: design an original funny, lovable little desk robot who keeps the user's coding tasks energized. A compact round mint-teal robot, oversized rounded-square head, deep midnight-navy glass face with two bright friendly curved eyes, a tiny asymmetric antenna with a warm amber tip, short chunky arms hugging one bold golden-yellow lightning bolt as if it were a favorite pillow. Subtle cheeky confident expression, a very slight jaunty tilt, charming but crisp and professional, memorable silhouette. Soft tactile ceramic and satin plastic 3D materials with restrained dimensional shading, warm soft studio light from upper left; the lightning is the only main accent. Front three-quarter view, centered, head and lightning visually dominate, no tiny buttons or mechanical details, instantly recognizable at Dock sizes. Background: an ivory/off-white macOS rounded-square icon tile, a very subtle mint glow behind the robot, subtle contact shadow entirely contained within the tile. One icon only, isolated square composition, transparent outside the rounded-square tile, generous even transparent outer margin about 9% of canvas; robot contained inside the tile. Make the face and lightning high-contrast and keep both readable at 32 pixels. No text, no letters, no numbers, no logos or OpenAI knot, no watermark, no mockup, no device frame, no labels, no separate panels.
```
