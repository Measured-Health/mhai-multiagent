"""
===========================================================================
MHAI Milestone M2 — Part 1 of 3
===========================================================================
Document Ingestion Agent (DIA): Swin Transformer V2 + MoE Routing
                                + SSIM Guardrail

Implements:
  - Swin-V2 backbone fine-tuned on PubLayNet for document layout analysis
  - MoE routing layer with 3 experts: OCR Text, Table TSR, Figure/Chart
  - Top-k gating network with load-balancing auxiliary loss
  - SSIM structural consistency guardrail (L1 variant, τ = 0.85)
  - DeepDoc-style ingestion pipeline: PDF → regions → structured chunks

Public data used:
  - PubLayNet (IBM, ~360K document images with layout annotations)
    Categories: text, title, list, table, figure
  - DocBank (Microsoft, 500K document pages — optional augmentation)

Requirements:
  pip install torch torchvision timm datasets transformers
  pip install scikit-image pdf2image Pillow pytesseract
  pip install detectron2  # for layout detection baseline
===========================================================================
"""

import torch
import torch.nn as nn
import torch.nn.functional as F
import numpy as np
from typing import List, Tuple, Dict, Optional, Any
from dataclasses import dataclass, field
from enum import Enum
import logging
import re

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger("MHAI.DIA")


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.1  DOCUMENT REGION TYPES AND DATA STRUCTURES
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class RegionType(Enum):
    """Document region categories from PubLayNet."""
    TEXT = 0
    TITLE = 1
    LIST = 2
    TABLE = 3
    FIGURE = 4


@dataclass
class DocumentRegion:
    """A detected region in a document page."""
    region_type: RegionType
    bbox: Tuple[float, float, float, float]  # (x1, y1, x2, y2) normalized
    confidence: float
    page_number: int = 0
    content: str = ""           # extracted text/data
    expert_used: str = ""       # which MoE expert processed this
    metadata: Dict[str, Any] = field(default_factory=dict)


@dataclass
class StructuredChunk:
    """Output of the DIA: a structured, semantically tagged chunk
    ready for RAGFlow KB ingestion."""
    chunk_id: str
    content: str
    chunk_type: str             # "text", "table", "figure_caption"
    page_number: int
    source_regions: List[DocumentRegion]
    ssim_score: float = 1.0     # structural consistency score
    passed_guardrail: bool = True
    metadata: Dict[str, Any] = field(default_factory=dict)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.2  SWIN TRANSFORMER V2 BACKBONE FOR LAYOUT ANALYSIS
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class SwinV2LayoutBackbone(nn.Module):
    """
    Swin Transformer V2 backbone for document layout analysis.

    Architecture (from Section 3.1 of the architecture blueprint):
      - Patch embedding: P×P = 4, projects image patches to d-dim tokens
      - 4 hierarchical stages with shifted-window attention:
        Stage 1: H/4 × W/4 × C
        Stage 2: H/8 × W/8 × 2C
        Stage 3: H/16 × W/16 × 4C
        Stage 4: H/32 × W/32 × 8C
      - Relative position bias via log-CPB for scale transfer:
        Attention(Q,K,V) = softmax(QK^T/√d + B) · V

    Uses timm (PyTorch Image Models) for the Swin-V2 backbone,
    fine-tuned on PubLayNet for 5-class layout detection.
    """

    def __init__(
        self,
        model_name: str = "swinv2_base_window12to16_192to256",
        num_classes: int = 5,  # PubLayNet: text, title, list, table, figure
        pretrained: bool = True,
        feature_dim: int = 1024
    ):
        super().__init__()
        import timm

        # Load pretrained Swin-V2 backbone
        self.backbone = timm.create_model(
            model_name,
            pretrained=pretrained,
            num_classes=0,          # Remove classification head
            features_only=True,     # Return multi-scale features
            out_indices=(0, 1, 2, 3)  # All 4 stages
        )

        self.feature_dim = feature_dim
        self.num_classes = num_classes

        # Feature Pyramid Network (FPN) for multi-scale fusion
        # Swin-V2 base output channels: [128, 256, 512, 1024]
        self.fpn_lateral = nn.ModuleList([
            nn.Conv2d(c, feature_dim, 1)
            for c in [128, 256, 512, 1024]
        ])
        self.fpn_output = nn.ModuleList([
            nn.Conv2d(feature_dim, feature_dim, 3, padding=1)
            for _ in range(4)
        ])

        # Region proposal + classification head
        self.region_classifier = nn.Sequential(
            nn.AdaptiveAvgPool2d(7),
            nn.Flatten(),
            nn.Linear(feature_dim * 49, 1024),
            nn.ReLU(inplace=True),
            nn.Dropout(0.3),
            nn.Linear(1024, num_classes)
        )

        # Bounding box regressor
        self.bbox_regressor = nn.Sequential(
            nn.AdaptiveAvgPool2d(7),
            nn.Flatten(),
            nn.Linear(feature_dim * 49, 1024),
            nn.ReLU(inplace=True),
            nn.Linear(1024, 4)  # (dx, dy, dw, dh)
        )

    def forward(
        self,
        x: torch.Tensor
    ) -> Tuple[torch.Tensor, List[torch.Tensor]]:
        """
        Forward pass through Swin-V2 + FPN.

        Args:
            x: Input image tensor (B, 3, H, W)

        Returns:
            class_logits: (B, num_classes) per-region class predictions
            multi_scale_features: List of feature maps at 4 scales
        """
        # Extract multi-scale features from Swin-V2
        features = self.backbone(x)  # List of 4 feature maps

        # FPN: top-down pathway with lateral connections
        fpn_features = []
        last_inner = self.fpn_lateral[-1](features[-1])
        fpn_features.append(self.fpn_output[-1](last_inner))

        for i in range(len(features) - 2, -1, -1):
            lateral = self.fpn_lateral[i](features[i])
            # Upsample and add
            upsampled = F.interpolate(
                last_inner, size=lateral.shape[-2:], mode='nearest'
            )
            last_inner = lateral + upsampled
            fpn_features.insert(0, self.fpn_output[i](last_inner))

        # Use finest-scale FPN feature for classification
        class_logits = self.region_classifier(fpn_features[0])
        bbox_delta = self.bbox_regressor(fpn_features[0])

        return class_logits, bbox_delta, fpn_features


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.3  MoE ROUTING LAYER: THREE EXPERTS
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class OCRTextExpert(nn.Module):
    """Expert 1: Processes text/title/list regions via OCR."""

    def __init__(self, feature_dim: int = 1024, output_dim: int = 512):
        super().__init__()
        self.proj = nn.Sequential(
            nn.Linear(feature_dim, output_dim),
            nn.ReLU(inplace=True),
            nn.Linear(output_dim, output_dim)
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.proj(x)

    def extract_text(self, image_region: np.ndarray) -> str:
        """
        Extract text from a document image region using Tesseract OCR.
        In production, replace with a transformer-based OCR like TrOCR.
        """
        try:
            import pytesseract
            from PIL import Image
            pil_image = Image.fromarray(image_region)
            text = pytesseract.image_to_string(
                pil_image,
                config='--oem 3 --psm 6'  # LSTM engine, block of text
            )
            return text.strip()
        except ImportError:
            logger.warning("pytesseract not available; returning placeholder")
            return "[OCR text extraction placeholder]"


class TableTSRExpert(nn.Module):
    """Expert 2: Processes table regions via Table Structure Recognition."""

    def __init__(self, feature_dim: int = 1024, output_dim: int = 512):
        super().__init__()
        self.proj = nn.Sequential(
            nn.Linear(feature_dim, output_dim),
            nn.ReLU(inplace=True),
            nn.Linear(output_dim, output_dim)
        )
        # Table structure heads: row/column detection
        self.row_detector = nn.Linear(output_dim, 1)  # row boundary score
        self.col_detector = nn.Linear(output_dim, 1)  # col boundary score

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.proj(x)

    def extract_table(self, image_region: np.ndarray) -> str:
        """
        Extract table structure from a document image region.
        Returns markdown-formatted table.
        In production, use TATR (Table Transformer) or similar.
        """
        try:
            import pytesseract
            from PIL import Image
            pil_image = Image.fromarray(image_region)
            # Use Tesseract TSV output for structured extraction
            tsv_data = pytesseract.image_to_data(
                pil_image, output_type=pytesseract.Output.DICT
            )
            # Simple heuristic: group by block_num and line_num
            lines = {}
            for i, text in enumerate(tsv_data['text']):
                if text.strip():
                    line_key = (tsv_data['block_num'][i],
                                tsv_data['line_num'][i])
                    if line_key not in lines:
                        lines[line_key] = []
                    lines[line_key].append(text.strip())

            # Format as markdown table
            rows = [" | ".join(words) for words in lines.values()]
            if len(rows) >= 2:
                header = rows[0]
                separator = " | ".join(
                    ["---"] * len(rows[0].split(" | "))
                )
                return "\n".join([header, separator] + rows[1:])
            return "\n".join(rows) if rows else "[Empty table]"
        except ImportError:
            return "[Table extraction placeholder]"


class FigureChartExpert(nn.Module):
    """Expert 3: Processes figure/chart regions."""

    def __init__(self, feature_dim: int = 1024, output_dim: int = 512):
        super().__init__()
        self.proj = nn.Sequential(
            nn.Linear(feature_dim, output_dim),
            nn.ReLU(inplace=True),
            nn.Linear(output_dim, output_dim)
        )
        # Figure type classifier
        self.figure_type_head = nn.Linear(
            output_dim, 4  # chart, diagram, photo, illustration
        )

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        return self.proj(x)

    def extract_caption(self, image_region: np.ndarray) -> str:
        """Extract figure caption (OCR on region below figure)."""
        try:
            import pytesseract
            from PIL import Image
            pil_image = Image.fromarray(image_region)
            text = pytesseract.image_to_string(pil_image)
            # Look for "Figure X:" or "Fig. X:" pattern
            caption_match = re.search(
                r'((?:Figure|Fig\.?)\s*\d+[.:].+)',
                text, re.IGNORECASE | re.DOTALL
            )
            if caption_match:
                return caption_match.group(1).strip()
            return f"[Figure detected, no caption extracted]"
        except ImportError:
            return "[Figure caption placeholder]"


class DIAMoERouter(nn.Module):
    """
    MoE Gating Network for the Document Ingestion Agent.

    Routes each detected document region to the appropriate expert:
      - Text/Title/List regions → OCR Text Expert
      - Table regions → Table TSR Expert
      - Figure regions → Figure/Chart Expert

    Gating function (from Section 3.5 of the architecture blueprint):
      G(x) = Softmax(TopK(W_g · x + ε)),  ε ~ N(0, σ²)

    Load-balancing auxiliary loss:
      L_bal = N · Σ_i f_i · P_i
    where f_i = fraction routed to expert i, P_i = mean gate prob for i.
    """

    def __init__(
        self,
        feature_dim: int = 1024,
        expert_dim: int = 512,
        num_experts: int = 3,
        top_k: int = 1,          # activate 1 expert per region
        noise_std: float = 0.1
    ):
        super().__init__()
        self.num_experts = num_experts
        self.top_k = top_k
        self.noise_std = noise_std

        # Gating network: feature → expert routing weights
        self.gate = nn.Sequential(
            nn.Linear(feature_dim, 256),
            nn.ReLU(inplace=True),
            nn.Linear(256, num_experts)
        )

        # Expert networks
        self.experts = nn.ModuleList([
            OCRTextExpert(feature_dim, expert_dim),
            TableTSRExpert(feature_dim, expert_dim),
            FigureChartExpert(feature_dim, expert_dim),
        ])

        # Expert names for logging
        self.expert_names = ["OCR_Text", "Table_TSR", "Figure_Chart"]

    def forward(
        self,
        region_features: torch.Tensor,
        region_types: Optional[torch.Tensor] = None
    ) -> Tuple[torch.Tensor, Dict[str, Any]]:
        """
        Route region features to experts.

        Args:
            region_features: (B, feature_dim) pooled features per region
            region_types: (B,) optional ground-truth types for supervised routing

        Returns:
            expert_outputs: (B, expert_dim) processed features
            routing_info: Dict with gating weights, load balance, etc.
        """
        batch_size = region_features.shape[0]

        # Compute gate logits with noise
        gate_logits = self.gate(region_features)
        if self.training:
            noise = torch.randn_like(gate_logits) * self.noise_std
            gate_logits = gate_logits + noise

        # Top-k selection
        gate_probs = F.softmax(gate_logits, dim=-1)
        topk_probs, topk_indices = torch.topk(
            gate_probs, self.top_k, dim=-1
        )
        # Renormalize top-k probabilities
        topk_probs = topk_probs / topk_probs.sum(dim=-1, keepdim=True)

        # Route to experts
        expert_outputs = torch.zeros(
            batch_size, self.experts[0].proj[0].in_features,
            device=region_features.device
        )
        # Simplified: use the output dim from first expert
        out_dim = 512
        expert_outputs = torch.zeros(
            batch_size, out_dim, device=region_features.device
        )

        for k in range(self.top_k):
            expert_idx = topk_indices[:, k]
            weight = topk_probs[:, k].unsqueeze(-1)

            for e_idx in range(self.num_experts):
                mask = (expert_idx == e_idx)
                if mask.any():
                    expert_input = region_features[mask]
                    expert_out = self.experts[e_idx](expert_input)
                    expert_outputs[mask] += weight[mask] * expert_out

        # Load-balancing auxiliary loss
        # f_i = fraction of tokens routed to expert i
        # P_i = mean gate probability for expert i
        f = torch.zeros(self.num_experts, device=region_features.device)
        for e_idx in range(self.num_experts):
            f[e_idx] = (topk_indices[:, 0] == e_idx).float().mean()
        P = gate_probs.mean(dim=0)
        load_balance_loss = self.num_experts * (f * P).sum()

        routing_info = {
            "gate_probs": gate_probs.detach(),
            "topk_indices": topk_indices.detach(),
            "load_balance_loss": load_balance_loss,
            "expert_distribution": {
                self.expert_names[i]: f[i].item()
                for i in range(self.num_experts)
            }
        }

        return expert_outputs, routing_info

    def route_region(
        self,
        region_feature: np.ndarray
    ) -> Tuple[int, str, float]:
        """
        Route a single region to an expert (inference mode).

        Returns: (expert_index, expert_name, confidence)
        """
        with torch.no_grad():
            x = torch.tensor(region_feature).unsqueeze(0).float()
            gate_logits = self.gate(x)
            gate_probs = F.softmax(gate_logits, dim=-1)
            expert_idx = gate_probs.argmax(dim=-1).item()
            confidence = gate_probs[0, expert_idx].item()

        return expert_idx, self.expert_names[expert_idx], confidence


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.4  SSIM STRUCTURAL CONSISTENCY GUARDRAIL
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class SSIMGuardrail:
    """
    SSIM Structural Consistency Check (L1 variant for DIA).

    After parsing, extracted text is re-rendered into a synthetic
    document image and compared against the original via pixel-level
    SSIM. Chunks with SSIM < τ_DIA = 0.85 are flagged for human review.

    From Section 3.1 of the architecture blueprint:
    "This is a domain-specific analogue of the persona vector: instead
    of probing the model's activation space, we probe the output fidelity
    of the vision pipeline."
    """

    def __init__(self, threshold: float = 0.85):
        self.threshold = threshold

    def compute_ssim(
        self,
        original_image: np.ndarray,
        reconstructed_image: np.ndarray,
        win_size: int = 7
    ) -> float:
        """
        Compute Structural Similarity Index (SSIM) between original
        and reconstructed document images.

        SSIM(x, y) = [l(x,y)]^α · [c(x,y)]^β · [s(x,y)]^γ
        where l = luminance, c = contrast, s = structure

        Uses scikit-image implementation.
        """
        try:
            from skimage.metrics import structural_similarity as ssim
            # Convert to grayscale if needed
            if len(original_image.shape) == 3:
                from skimage.color import rgb2gray
                original_image = rgb2gray(original_image)
            if len(reconstructed_image.shape) == 3:
                from skimage.color import rgb2gray
                reconstructed_image = rgb2gray(reconstructed_image)

            # Resize to match if dimensions differ
            if original_image.shape != reconstructed_image.shape:
                from skimage.transform import resize
                reconstructed_image = resize(
                    reconstructed_image,
                    original_image.shape,
                    anti_aliasing=True
                )

            # Ensure win_size is odd and <= min dimension
            min_dim = min(original_image.shape)
            if win_size > min_dim:
                win_size = min_dim if min_dim % 2 == 1 else min_dim - 1
            if win_size < 3:
                win_size = 3

            score = ssim(
                original_image,
                reconstructed_image,
                win_size=win_size,
                data_range=1.0
            )
            return float(score)

        except ImportError:
            logger.warning(
                "scikit-image not available; using simplified SSIM"
            )
            return self._simplified_ssim(
                original_image, reconstructed_image
            )

    def _simplified_ssim(
        self,
        img1: np.ndarray,
        img2: np.ndarray
    ) -> float:
        """Simplified SSIM fallback without scikit-image."""
        # Ensure same shape
        if img1.shape != img2.shape:
            min_h = min(img1.shape[0], img2.shape[0])
            min_w = min(img1.shape[1], img2.shape[1])
            img1 = img1[:min_h, :min_w]
            img2 = img2[:min_h, :min_w]

        img1 = img1.astype(np.float64)
        img2 = img2.astype(np.float64)

        c1 = (0.01 * 255) ** 2
        c2 = (0.03 * 255) ** 2

        mu1 = img1.mean()
        mu2 = img2.mean()
        sigma1_sq = img1.var()
        sigma2_sq = img2.var()
        sigma12 = ((img1 - mu1) * (img2 - mu2)).mean()

        ssim_val = (
            (2 * mu1 * mu2 + c1) * (2 * sigma12 + c2)
        ) / (
            (mu1 ** 2 + mu2 ** 2 + c1) * (sigma1_sq + sigma2_sq + c2)
        )
        return float(ssim_val)

    def render_text_to_image(
        self,
        text: str,
        page_width: int = 800,
        page_height: int = 1100,
        font_size: int = 12
    ) -> np.ndarray:
        """
        Render extracted text back into a synthetic document image.
        Used for round-trip SSIM verification.
        """
        try:
            from PIL import Image, ImageDraw, ImageFont
            img = Image.new('L', (page_width, page_height), 255)
            draw = ImageDraw.Draw(img)

            try:
                font = ImageFont.truetype(
                    "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
                    font_size
                )
            except (IOError, OSError):
                font = ImageFont.load_default()

            # Simple text rendering with line wrapping
            y_pos = 20
            max_width = page_width - 40
            words = text.split()
            line = ""

            for word in words:
                test_line = f"{line} {word}".strip()
                bbox = draw.textbbox((0, 0), test_line, font=font)
                w = bbox[2] - bbox[0]
                if w <= max_width:
                    line = test_line
                else:
                    draw.text((20, y_pos), line, fill=0, font=font)
                    y_pos += font_size + 4
                    line = word
                    if y_pos > page_height - 40:
                        break

            if line:
                draw.text((20, y_pos), line, fill=0, font=font)

            return np.array(img)

        except ImportError:
            # Fallback: create a simple pixel pattern from text
            img = np.ones((page_height, page_width), dtype=np.uint8) * 255
            return img

    def check(
        self,
        original_image: np.ndarray,
        extracted_text: str,
        region_bbox: Optional[Tuple[float, float, float, float]] = None
    ) -> Tuple[float, bool, str]:
        """
        Run the SSIM guardrail check.

        Args:
            original_image: Original document page image
            extracted_text: Text extracted by the DIA
            region_bbox: Optional bounding box to crop the comparison region

        Returns:
            (ssim_score, passed, message)
        """
        # Crop to region if bbox provided
        if region_bbox is not None:
            h, w = original_image.shape[:2]
            x1 = int(region_bbox[0] * w)
            y1 = int(region_bbox[1] * h)
            x2 = int(region_bbox[2] * w)
            y2 = int(region_bbox[3] * h)
            original_crop = original_image[y1:y2, x1:x2]
        else:
            original_crop = original_image

        # Render extracted text to synthetic image
        crop_h, crop_w = original_crop.shape[:2]
        rendered = self.render_text_to_image(
            extracted_text,
            page_width=max(100, crop_w),
            page_height=max(100, crop_h)
        )

        # Compute SSIM
        ssim_score = self.compute_ssim(original_crop, rendered)
        passed = ssim_score >= self.threshold

        if passed:
            message = f"SSIM={ssim_score:.4f} >= {self.threshold} → PASS"
        else:
            message = (
                f"SSIM={ssim_score:.4f} < {self.threshold} → FLAGGED "
                f"for human review (possible extraction error)"
            )

        return ssim_score, passed, message


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.5  PubLayNet TRAINING PIPELINE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class PubLayNetDataset(torch.utils.data.Dataset):
    """
    Dataset wrapper for PubLayNet (IBM, 2019).

    PubLayNet contains ~360K document images from PubMed articles
    with layout annotations in COCO format:
      - text (330K instances)
      - title (38K instances)
      - list (6.5K instances)
      - table (78K instances)
      - figure (98K instances)

    Each image has bounding boxes and category labels for all regions.
    """

    def __init__(
        self,
        split: str = "train",
        max_samples: int = 1000,
        image_size: int = 256,
        transform=None
    ):
        self.split = split
        self.max_samples = max_samples
        self.image_size = image_size
        self.transform = transform

        # Category mapping
        self.category_map = {
            1: RegionType.TEXT,
            2: RegionType.TITLE,
            3: RegionType.LIST,
            4: RegionType.TABLE,
            5: RegionType.FIGURE,
        }

        self.samples = []
        self._load_dataset()

    def _load_dataset(self):
        """Load PubLayNet from Hugging Face datasets."""
        try:
            from datasets import load_dataset
            ds = load_dataset(
                "lhoestq/publaynet",
                split=f"{self.split}[:{self.max_samples}]"
            )

            for item in ds:
                image = item["image"]
                annotations = item.get("annotations", {})
                bboxes = annotations.get("bbox", [])
                categories = annotations.get("category_id", [])

                regions = []
                for bbox, cat_id in zip(bboxes, categories):
                    if cat_id in self.category_map:
                        regions.append({
                            "bbox": bbox,  # [x, y, w, h] COCO format
                            "category": self.category_map[cat_id]
                        })

                self.samples.append({
                    "image": image,
                    "regions": regions
                })

            logger.info(
                f"Loaded {len(self.samples)} PubLayNet samples "
                f"({self.split})"
            )

        except Exception as e:
            logger.warning(f"PubLayNet loading failed: {e}")
            logger.info("Generating synthetic document samples")
            self._generate_synthetic()

    def _generate_synthetic(self):
        """Generate synthetic document layout samples for testing."""
        from PIL import Image, ImageDraw

        for i in range(min(50, self.max_samples)):
            # Create a synthetic document page
            img = Image.new('RGB', (800, 1100), 'white')
            draw = ImageDraw.Draw(img)
            regions = []

            # Title region
            draw.rectangle([50, 30, 750, 80], outline='black')
            draw.text((60, 45), f"Document Title {i+1}", fill='black')
            regions.append({
                "bbox": [50, 30, 700, 50],
                "category": RegionType.TITLE
            })

            # Text regions
            for row in range(3):
                y = 120 + row * 200
                draw.rectangle([50, y, 750, y + 160], outline='gray')
                for line in range(8):
                    draw.text(
                        (60, y + 10 + line * 18),
                        f"Sample text line {line+1} of paragraph {row+1}",
                        fill='black'
                    )
                regions.append({
                    "bbox": [50, y, 700, 160],
                    "category": RegionType.TEXT
                })

            # Table region
            draw.rectangle([50, 760, 400, 920], outline='black', width=2)
            for r in range(4):
                for c in range(3):
                    x = 50 + c * 117
                    y = 760 + r * 40
                    draw.rectangle([x, y, x + 117, y + 40], outline='gray')
                    draw.text((x + 5, y + 10), f"Cell {r},{c}", fill='black')
            regions.append({
                "bbox": [50, 760, 350, 160],
                "category": RegionType.TABLE
            })

            # Figure region
            draw.rectangle([450, 760, 750, 920], outline='blue', width=2)
            draw.text((500, 830), "[Figure]", fill='blue')
            regions.append({
                "bbox": [450, 760, 300, 160],
                "category": RegionType.FIGURE
            })

            self.samples.append({
                "image": img,
                "regions": regions
            })

    def __len__(self):
        return len(self.samples)

    def __getitem__(self, idx):
        sample = self.samples[idx]
        image = sample["image"]

        # Convert PIL to numpy if needed
        if hasattr(image, 'convert'):
            image = np.array(image.convert('RGB'))

        # Resize
        from PIL import Image
        pil_img = Image.fromarray(image)
        pil_img = pil_img.resize(
            (self.image_size, self.image_size),
            Image.BILINEAR
        )
        image = np.array(pil_img)

        # Normalize to [0, 1] and convert to tensor
        image_tensor = torch.tensor(
            image, dtype=torch.float32
        ).permute(2, 0, 1) / 255.0

        # Prepare region labels
        region_labels = []
        region_bboxes = []
        for region in sample["regions"]:
            region_labels.append(region["category"].value)
            region_bboxes.append(region["bbox"])

        return {
            "image": image_tensor,
            "labels": torch.tensor(region_labels, dtype=torch.long),
            "bboxes": torch.tensor(region_bboxes, dtype=torch.float32),
            "num_regions": len(region_labels)
        }


class SwinV2Trainer:
    """
    Training pipeline for Swin-V2 fine-tuning on PubLayNet.

    Training config:
      - Optimizer: AdamW (lr=1e-4, weight_decay=0.05)
      - Scheduler: Cosine annealing with warmup
      - Augmentation: RandomResizedCrop, HorizontalFlip, ColorJitter
      - Epochs: 20 (with early stopping patience=5)
      - Batch size: 16
    """

    def __init__(
        self,
        model: SwinV2LayoutBackbone,
        moe_router: DIAMoERouter,
        lr: float = 1e-4,
        weight_decay: float = 0.05,
        load_balance_coeff: float = 0.01,
        device: str = "cuda" if torch.cuda.is_available() else "cpu"
    ):
        self.model = model.to(device)
        self.moe_router = moe_router.to(device)
        self.device = device
        self.lb_coeff = load_balance_coeff

        self.optimizer = torch.optim.AdamW(
            list(model.parameters()) + list(moe_router.parameters()),
            lr=lr,
            weight_decay=weight_decay
        )
        self.scheduler = torch.optim.lr_scheduler.CosineAnnealingLR(
            self.optimizer, T_max=20
        )
        self.criterion = nn.CrossEntropyLoss()

    def train_epoch(
        self,
        dataloader: torch.utils.data.DataLoader
    ) -> Dict[str, float]:
        """Train for one epoch."""
        self.model.train()
        self.moe_router.train()

        total_loss = 0
        total_cls_loss = 0
        total_lb_loss = 0
        correct = 0
        total = 0

        for batch in dataloader:
            images = batch["image"].to(self.device)

            # Forward through Swin-V2 backbone
            class_logits, bbox_delta, fpn_features = self.model(images)

            # Pool features for MoE routing
            pooled_features = F.adaptive_avg_pool2d(
                fpn_features[0], 1
            ).flatten(1)

            # Route through MoE
            expert_out, routing_info = self.moe_router(pooled_features)

            # Classification loss (use first region label per image)
            labels = batch["labels"][:, 0].to(self.device)
            valid = labels < self.model.num_classes
            if valid.any():
                cls_loss = self.criterion(
                    class_logits[valid], labels[valid]
                )
            else:
                cls_loss = torch.tensor(0.0, device=self.device)

            # Load-balance loss
            lb_loss = routing_info["load_balance_loss"]

            # Total loss
            loss = cls_loss + self.lb_coeff * lb_loss
            loss.backward()
            self.optimizer.step()
            self.optimizer.zero_grad()

            total_loss += loss.item()
            total_cls_loss += cls_loss.item()
            total_lb_loss += lb_loss.item()

            if valid.any():
                preds = class_logits[valid].argmax(dim=-1)
                correct += (preds == labels[valid]).sum().item()
                total += valid.sum().item()

        self.scheduler.step()

        return {
            "total_loss": total_loss / max(len(dataloader), 1),
            "cls_loss": total_cls_loss / max(len(dataloader), 1),
            "lb_loss": total_lb_loss / max(len(dataloader), 1),
            "accuracy": correct / max(total, 1),
            "expert_distribution": routing_info["expert_distribution"]
        }


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.6  FULL DIA INGESTION PIPELINE
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

class DocumentIngestionAgent:
    """
    Full Document Ingestion Agent pipeline:
      PDF → page images → Swin-V2 layout detection → MoE expert routing
      → text/table/figure extraction → SSIM guardrail → structured chunks

    This is the complete DIA from the architecture blueprint.
    """

    def __init__(
        self,
        swin_backbone: Optional[SwinV2LayoutBackbone] = None,
        moe_router: Optional[DIAMoERouter] = None,
        ssim_guardrail: Optional[SSIMGuardrail] = None,
        device: str = "cpu"
    ):
        self.backbone = swin_backbone
        self.router = moe_router or DIAMoERouter()
        self.guardrail = ssim_guardrail or SSIMGuardrail(threshold=0.85)
        self.device = device

        # Standalone expert instances for text extraction
        self.ocr_expert = OCRTextExpert()
        self.tsr_expert = TableTSRExpert()
        self.fig_expert = FigureChartExpert()

    def ingest_pdf(
        self,
        pdf_path: str,
        dpi: int = 200
    ) -> List[StructuredChunk]:
        """
        Full PDF ingestion pipeline.

        Args:
            pdf_path: Path to the input PDF
            dpi: Resolution for page rasterization

        Returns:
            List of StructuredChunk ready for RAGFlow KB
        """
        # Step 1: Convert PDF to page images
        page_images = self._pdf_to_images(pdf_path, dpi)
        logger.info(f"Converted {pdf_path} to {len(page_images)} page images")

        all_chunks = []
        chunk_id = 0

        for page_num, page_image in enumerate(page_images):
            # Step 2: Detect layout regions
            regions = self._detect_regions(page_image, page_num)
            logger.info(
                f"  Page {page_num + 1}: {len(regions)} regions detected"
            )

            # Step 3: Route each region to expert and extract content
            for region in regions:
                content = self._extract_content(region, page_image)
                region.content = content

            # Step 4: SSIM guardrail check
            for region in regions:
                if region.content and region.region_type in (
                    RegionType.TEXT, RegionType.TITLE, RegionType.LIST
                ):
                    ssim_score, passed, msg = self.guardrail.check(
                        page_image, region.content, region.bbox
                    )
                else:
                    ssim_score, passed = 1.0, True

                # Step 5: Create structured chunk
                chunk = StructuredChunk(
                    chunk_id=f"doc_{chunk_id:06d}",
                    content=region.content,
                    chunk_type=region.region_type.name.lower(),
                    page_number=page_num + 1,
                    source_regions=[region],
                    ssim_score=ssim_score,
                    passed_guardrail=passed,
                    metadata={
                        "expert_used": region.expert_used,
                        "confidence": region.confidence,
                        "bbox": region.bbox
                    }
                )
                all_chunks.append(chunk)
                chunk_id += 1

        # Summary
        n_passed = sum(1 for c in all_chunks if c.passed_guardrail)
        n_flagged = len(all_chunks) - n_passed
        logger.info(
            f"Ingestion complete: {len(all_chunks)} chunks "
            f"({n_passed} passed, {n_flagged} flagged)"
        )

        return all_chunks

    def _pdf_to_images(
        self,
        pdf_path: str,
        dpi: int = 200
    ) -> List[np.ndarray]:
        """Convert PDF pages to images."""
        try:
            from pdf2image import convert_from_path
            pil_images = convert_from_path(pdf_path, dpi=dpi)
            return [np.array(img) for img in pil_images]
        except ImportError:
            logger.warning("pdf2image not available; generating synthetic page")
            return [self._generate_synthetic_page()]

    def _generate_synthetic_page(self) -> np.ndarray:
        """Generate a synthetic document page for testing."""
        from PIL import Image, ImageDraw
        img = Image.new('RGB', (800, 1100), 'white')
        draw = ImageDraw.Draw(img)
        draw.text((50, 30), "Synthetic Document Page", fill='black')
        draw.text((50, 100), "This is sample body text for testing.", fill='black')
        draw.rectangle([50, 300, 400, 500], outline='black')
        draw.text((60, 310), "Table header | Col 1 | Col 2", fill='black')
        return np.array(img)

    def _detect_regions(
        self,
        page_image: np.ndarray,
        page_number: int
    ) -> List[DocumentRegion]:
        """
        Detect layout regions in a page image.
        In production, uses Swin-V2 backbone. Here uses heuristic fallback.
        """
        h, w = page_image.shape[:2]

        # Simple heuristic region detection for demo
        regions = [
            DocumentRegion(
                region_type=RegionType.TITLE,
                bbox=(0.05, 0.02, 0.95, 0.08),
                confidence=0.92,
                page_number=page_number
            ),
            DocumentRegion(
                region_type=RegionType.TEXT,
                bbox=(0.05, 0.10, 0.95, 0.55),
                confidence=0.88,
                page_number=page_number
            ),
            DocumentRegion(
                region_type=RegionType.TABLE,
                bbox=(0.05, 0.58, 0.50, 0.82),
                confidence=0.85,
                page_number=page_number
            ),
            DocumentRegion(
                region_type=RegionType.FIGURE,
                bbox=(0.55, 0.58, 0.95, 0.82),
                confidence=0.90,
                page_number=page_number
            ),
        ]
        return regions

    def _extract_content(
        self,
        region: DocumentRegion,
        page_image: np.ndarray
    ) -> str:
        """Route region to the appropriate expert for content extraction."""
        h, w = page_image.shape[:2]
        x1 = int(region.bbox[0] * w)
        y1 = int(region.bbox[1] * h)
        x2 = int(region.bbox[2] * w)
        y2 = int(region.bbox[3] * h)
        crop = page_image[y1:y2, x1:x2]

        if region.region_type in (
            RegionType.TEXT, RegionType.TITLE, RegionType.LIST
        ):
            region.expert_used = "OCR_Text"
            return self.ocr_expert.extract_text(crop)
        elif region.region_type == RegionType.TABLE:
            region.expert_used = "Table_TSR"
            return self.tsr_expert.extract_table(crop)
        elif region.region_type == RegionType.FIGURE:
            region.expert_used = "Figure_Chart"
            return self.fig_expert.extract_caption(crop)
        else:
            region.expert_used = "OCR_Text"
            return self.ocr_expert.extract_text(crop)


# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# 1.7  DEMO
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

def demo_dia_pipeline():
    """Demonstrate the full DIA pipeline."""
    print("=" * 70)
    print("MHAI M2 Part 1: Document Ingestion Agent (DIA)")
    print("=" * 70)

    # ── Step 1: PubLayNet dataset ──
    print("\n[1/5] Loading PubLayNet dataset...")
    dataset = PubLayNetDataset(split="train", max_samples=10)
    print(f"  Loaded {len(dataset)} samples")
    if len(dataset) > 0:
        sample = dataset[0]
        print(f"  Image shape: {sample['image'].shape}")
        print(f"  Regions: {sample['num_regions']}")

    # ── Step 2: Model architecture ──
    print("\n[2/5] Initializing Swin-V2 + MoE architecture...")
    print("  Swin-V2 backbone: swinv2_base_window12to16")
    print("  FPN: 4-level feature pyramid (128→256→512→1024)")
    print("  MoE: 3 experts (OCR, TSR, Figure), top-1 gating")

    router = DIAMoERouter(feature_dim=1024, expert_dim=512, num_experts=3)
    n_params = sum(p.numel() for p in router.parameters())
    print(f"  MoE router parameters: {n_params:,}")

    # ── Step 3: MoE routing demo ──
    print("\n[3/5] MoE routing demonstration...")
    mock_features = torch.randn(6, 1024)
    with torch.no_grad():
        expert_out, routing_info = router(mock_features)

    print(f"  Input: 6 region features (dim=1024)")
    print(f"  Output: {expert_out.shape}")
    print(f"  Expert distribution:")
    for name, frac in routing_info["expert_distribution"].items():
        bar = "█" * int(frac * 30)
        print(f"    {name:<15} {bar} {frac:.1%}")
    print(f"  Load-balance loss: {routing_info['load_balance_loss']:.4f}")

    # ── Step 4: SSIM guardrail ──
    print("\n[4/5] SSIM guardrail testing...")
    guardrail = SSIMGuardrail(threshold=0.85)

    # Test case 1: Good extraction (high SSIM)
    original = np.random.randint(200, 255, (200, 400), dtype=np.uint8)
    good_reconstruction = original.copy()
    good_reconstruction = good_reconstruction + np.random.randint(
        -5, 5, original.shape, dtype=np.int16
    )
    good_reconstruction = np.clip(good_reconstruction, 0, 255).astype(np.uint8)

    score1 = guardrail.compute_ssim(original, good_reconstruction)
    print(f"  Good extraction:  SSIM={score1:.4f} "
          f"{'✅ PASS' if score1 >= 0.85 else '❌ FAIL'}")

    # Test case 2: Bad extraction (low SSIM)
    bad_reconstruction = np.random.randint(0, 255, original.shape, dtype=np.uint8)
    score2 = guardrail.compute_ssim(original, bad_reconstruction)
    print(f"  Bad extraction:   SSIM={score2:.4f} "
          f"{'✅ PASS' if score2 >= 0.85 else '❌ FAIL'}")

    # Test case 3: Partial corruption
    partial = original.copy()
    partial[50:150, 100:300] = np.random.randint(
        0, 255, (100, 200), dtype=np.uint8
    )
    score3 = guardrail.compute_ssim(original, partial)
    print(f"  Partial corrupt:  SSIM={score3:.4f} "
          f"{'✅ PASS' if score3 >= 0.85 else '❌ FAIL'}")

    # ── Step 5: Full ingestion pipeline ──
    print("\n[5/5] Full DIA ingestion pipeline (synthetic page)...")
    dia = DocumentIngestionAgent()
    page_img = dia._generate_synthetic_page()
    regions = dia._detect_regions(page_img, 0)
    print(f"  Detected {len(regions)} regions:")
    for r in regions:
        print(f"    {r.region_type.name:<10} conf={r.confidence:.2f} "
              f"bbox={r.bbox}")

    print("\n" + "=" * 70)
    print("M2 Part 1 complete. Proceed to Part 2 for the Quick Detection")
    print("Agent (QDA) with FT-Transformer + RUSBoost calibration?")
    print("=" * 70)


if __name__ == "__main__":
    demo_dia_pipeline()